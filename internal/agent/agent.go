package agent

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Loop de execução (epic §5): poll → write → ack, com as réguas do contrato:
//
//   - ordem do lote preservada (decisão 13 — nunca reordenar);
//   - todo poll bem-sucedido é heartbeat (inclusive o vazio);
//   - ACK só depois do write completo; write falhou → quebra o lote, sem
//     ack (comanda pela metade não existe);
//   - ACK falhou (rede ou 4xx) → loga e segue o lote (at-least-once);
//   - backoff exponencial SÓ em falha de poll/rede/servidor; 404 → 30s
//     fixo; falha de impressão não afasta o poll (heartbeat fresco);
//   - config nova (config_version mudou) aplica só entre rodadas;
//   - configured=false congela a escrita, o poll segue (heartbeat);
//   - auto-update só em rodada idle (poll vazio), nunca no meio de um lote.
const (
	NoTokenRetry    = 30 * time.Second // epic §3: token.txt pode chegar depois; nunca sai
	NotFoundRetry   = 30 * time.Second // 404 = token/flag — fixo, sem backoff (epic §5)
	maxBackoff      = 60 * time.Second
	defaultInterval = 3 * time.Second // poll_interval ausente/≤0 → 3s
	minInterval     = 1 * time.Second // clamp [1s, 60s]
	maxInterval     = 60 * time.Second
	LockRetryWindow = 30 * time.Second // retry do lock pós-update (epic §7.4)

	// Diagnóstico local (decisão: log completo no arquivo, §3): o silêncio
	// de um loop vivo não pode ser confundido com loop morto, e estados
	// estáticos não podem encher o arquivo de linha idêntica.
	heartbeatGap = 5 * time.Minute // rodada idle loga resumo a cada 5 min (a 1ª loga na hora)
	noiseGap     = 5 * time.Minute // estado estático (configured=false, 404, token) repete só a cada 5 min

	// Teto do write na impressora (spooler Windows e lp/CUPS): write sem
	// deadline congelaria o loop inteiro num spooler travado. Comanda
	// pequena em fila saudável sai em <1s; 30s é margem folgada — passou
	// disso, o problema é infra (mesmo tradeoff de ack falho: sem ack,
	// reimprime; o abandoned do Windows pode imprimir em dobro).
	usbWriteTimeout = 30 * time.Second
)

type Writer interface {
	Write(ctx context.Context, cfg DeviceConfig, payload []byte) error
}

// USBWriter despacha para o writer de spooler/fila RAW do OS (matriz epic §2).
type USBWriter struct{}

func (USBWriter) Write(ctx context.Context, cfg DeviceConfig, payload []byte) error {
	if cfg.PrinterName == "" {
		return errors.New("conexão usb sem printer_name na config")
	}
	return writeUSBPlatform(ctx, cfg.PrinterName, payload)
}

type Agent struct {
	RC      *RuntimeConfig
	Client  *Client
	Log     *Logger
	Dbg     *DebugBuffer
	Usb     Writer
	Updater *Updater

	// estado da sessão (tudo em memória; negócio nenhum depende do relógio local)
	Cur           DeviceConfig
	HaveConfig    bool
	PendingConfig *DeviceConfig // aplica entre rodadas (epic §5)
	Backoff       time.Duration

	Sleep func(time.Duration) // injetável em testes; nil = sleep real com ctx
	Now   func() time.Time    // injetável em testes; nil = time.Now

	// diagnóstico de sessão (log completo no arquivo, §3): contadores que
	// alimentam o heartbeat de idle e o resumo do encerramento; throttle
	// dos estados estáticos (uma linha por noiseGap, com suprimidas).
	startedAt   time.Time
	printedOK   int // write completo + ack ok
	printFailed int // write quebrou o lote (sem ack)
	ackLost     int // escrito mas ack falhou (reimprime no próximo poll)
	lastHeart   time.Time
	noise       map[string]*noiseCtl
}

// noiseCtl rastreia um estado estático para o logNoise (throttle por chave).
type noiseCtl struct {
	last       time.Time
	suppressed int
}

func NewAgent(rc *RuntimeConfig, client *Client, log *Logger, dbg *DebugBuffer, upd *Updater) *Agent {
	now := time.Now()
	return &Agent{
		RC:        rc,
		Client:    client,
		Log:       log,
		Dbg:       dbg,
		Usb:       USBWriter{},
		Updater:   upd,
		startedAt: now,
		lastHeart: now.Add(-heartbeatGap), // 1ª rodada idle já loga o heartbeat
	}
}

func (a *Agent) now() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now()
}

func ClampInterval(seconds int) time.Duration {
	if seconds <= 0 {
		return defaultInterval
	}
	d := time.Duration(seconds) * time.Second
	if d < minInterval {
		return minInterval
	}
	if d > maxInterval {
		return maxInterval
	}
	return d
}

func StatusOf(err error) int {
	var he *HTTPError
	if errors.As(err, &he) {
		return he.Status
	}
	return 0 // falha de rede/timeout
}

func minDur(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}

func (a *Agent) sleepCtx(ctx context.Context, d time.Duration) bool {
	if a.Sleep != nil {
		a.Sleep(d)
		return ctx.Err() == nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func (a *Agent) baseInterval() time.Duration {
	if a.HaveConfig {
		return ClampInterval(a.Cur.PollInterval)
	}
	return defaultInterval
}

// sessionSuffix alimenta heartbeat e resumo de encerramento com os números
// da sessão (o "imprimiu quantas?" se responde pelo log, sem ir ao painel).
func (a *Agent) sessionSuffix() string {
	return fmt.Sprintf("sessão %s · %d job(s) ok · %d falha(s) de escrita · %d ack perdido(s)",
		a.now().Sub(a.startedAt).Truncate(time.Second), a.printedOK, a.printFailed, a.ackLost)
}

// logNoise: estado estático (token ausente, configured=false, 404 fixo) não
// traz informação nova a cada 3s/30s — a 1ª ocorrência loga, as seguintes
// só depois de noiseGap, e o re-disparo carrega quantas foram suprimidas.
// Vai pelo buffer de debug (o espelho Sink leva ao agent.log) — uma linha
// por disparo, sem duplicar no arquivo.
func (a *Agent) logNoise(key string, format string, args ...any) {
	if a.noise == nil {
		a.noise = make(map[string]*noiseCtl)
	}
	n := a.noise[key]
	now := a.now()
	if n == nil || now.Sub(n.last) >= noiseGap {
		if n != nil && n.suppressed > 0 {
			format += " — %d ocorrência(s) igual(is) suprimida(s) desde a última linha"
			args = append(args, n.suppressed)
		}
		a.Dbg.Addf(format, args...)
		if n == nil {
			n = &noiseCtl{}
			a.noise[key] = n
		}
		n.last = now
		n.suppressed = 0
		return
	}
	n.suppressed++
}

func describeTarget(cfg DeviceConfig) string {
	switch cfg.Connection {
	case "usb":
		if cfg.PrinterName == "" {
			return "usb (sem printer_name!)"
		}
		return "usb · " + cfg.PrinterName
	case "network":
		port := cfg.Port
		if port <= 0 {
			port = DefaultPrintPort
		}
		return "network · " + cfg.Host + ":" + strconv.Itoa(port)
	default:
		return "connection=" + strconv.Quote(cfg.Connection)
	}
}

// Run executa o boot em 4 passos (epic §4) e entra no loop (§5). Sai com
// o ctx cancelado (shutdown limpo — pendentes ficam no servidor).
func (a *Agent) Run(ctx context.Context) {
	if !a.TokenGate(ctx) {
		return
	}
	if !a.ReportGate(ctx) {
		return
	}
	if !a.ConfigGate(ctx) {
		return
	}
	a.loop(ctx)
}

// ---------- passo 1: token ----------

func (a *Agent) TokenGate(ctx context.Context) bool {
	for {
		if tok := ReadToken(a.RC.TokenPath); tok != "" {
			return true
		}
		a.logNoise("token", "token.txt ausente ou vazio — gere o token no card Impressora do quadro de pedidos (modo agent) e cole no arquivo %s; retry em 30s (o agente não sai)", a.RC.TokenPath)
		if !a.sleepCtx(ctx, NoTokenRetry) {
			return false
		}
	}
}

// ---------- passo 2: report de impressoras ----------

// ReportGate posta o snapshot até o primeiro sucesso (200) — o passo 3 só
// começa depois (gate do epic §4). Falhou: backoff exponencial (base do
// intervalo, teto 60s); 404 = token/flag → retry fixo 30s. A coleta roda
// UMA vez antes do POST (o report é snapshot do boot — retry de rede/404
// não re-enumera o spooler: falha de auth não muda o que o OS enxerga, e
// enumeração a cada 30s por horas era lixo). As notas da coleta e as linhas
// do gate vão só pelo buffer (o espelho Sink as leva ao agent.log, sem
// dupla linha) — nunca "0 coletada(s)" sem motivo, e sem spam em retry.
func (a *Agent) ReportGate(ctx context.Context) bool {
	backoff := time.Duration(0)
	res := CollectPrinters(ctx)
	printers := res.Kept
	if len(res.Notes) > 0 {
		a.Dbg.Add("boot passo 2: " + strings.Join(res.Notes, "; "))
	}
	for {
		stored, err := a.Client.ReportPrinters(ctx, BuildReportBody(printers))
		if err == nil {
			a.Dbg.Addf("boot passo 2 ok: report aceito: %d entrada(s) armazenada(s) (de %d coletada(s) do sistema; %d após filtro)", stored, res.RawCount, len(printers))
			return true
		}
		if StatusOf(err) == 404 {
			a.logNoise("report404", "report: 404 — token rejeitado ou comandas desligadas; retry em 30s (troque o token.txt ou ligue as comandas no painel)")
			if !a.sleepCtx(ctx, NotFoundRetry) {
				return false
			}
			continue
		}
		if backoff == 0 {
			backoff = defaultInterval
		} else {
			backoff = minDur(backoff*2, maxBackoff)
		}
		a.Dbg.Addf("boot passo 2 falhou (%v) — retry em %s", err, backoff)
		if !a.sleepCtx(ctx, backoff) {
			return false
		}
	}
}

// ---------- passo 3: config ----------

// ConfigGate consulta a config até configured=true. 404 → retry fixo 30s
// (não conta heartbeat no servidor — device com flag desligada fica vermelho
// no card até ligarem, e é esperado). configured=false → reconsulta no
// intervalo (esse GET conta heartbeat) sem escrever em impressora nenhuma.
func (a *Agent) ConfigGate(ctx context.Context) bool {
	backoff := time.Duration(0)
	for {
		cfg, err := a.Client.GetConfig(ctx)
		if err != nil {
			if StatusOf(err) == 404 {
				a.logNoise("config404", "config: 404 — token rejeitado ou comandas desligadas; retry em 30s (troque o token.txt ou ligue as comandas)")
				if !a.sleepCtx(ctx, NotFoundRetry) {
					return false
				}
				continue
			}
			if backoff == 0 {
				backoff = defaultInterval
			} else {
				backoff = minDur(backoff*2, maxBackoff)
			}
			a.Log.Printf("config falhou (%v) — retry em %s", err, backoff)
			if !a.sleepCtx(ctx, backoff) {
				return false
			}
			continue
		}
		backoff = 0
		if !cfg.Configured {
			a.logNoise("cfgnotready", "configured=false — impressora ainda não completada no painel; aguardando (consulta conta heartbeat, nada escreve)")
			if !a.sleepCtx(ctx, ClampInterval(cfg.PollInterval)) {
				return false
			}
			continue
		}
		a.applyConfig(cfg)
		return true
	}
}

// applyConfig aplica a config nova sem reiniciar o processo (epic §5).
func (a *Agent) applyConfig(c *DeviceConfig) {
	a.Cur = *c
	a.HaveConfig = true
	a.PendingConfig = nil
	a.Dbg.Addf("config aplicada: %s · intervalo %ds · debug %v · cfg %s",
		describeTarget(*c), c.PollInterval, c.DebugMode, c.ConfigVersion)
	a.Log.Printf("config vigente: %s · intervalo %ds · debug %v", describeTarget(*c), c.PollInterval, c.DebugMode)
}

// ---------- passo 4: loop ----------

type RoundResult struct {
	Sleep time.Duration
	Exit  bool // update fez spawn da instância nova — encerrar
}

func (a *Agent) loop(ctx context.Context) {
	for {
		res := a.Round(ctx)
		if res.Exit {
			// Update fez spawn da instância nova — o "porquê" já foi logado
			// pelo updater; aqui vai só o resumo da sessão que termina.
			a.Dbg.Addf("encerrando para o update — %s", a.sessionSuffix())
			return
		}
		if !a.sleepCtx(ctx, res.Sleep) {
			a.Dbg.Addf("encerrando — jobs pendentes ficam no servidor (at-least-once) · %s", a.sessionSuffix())
			return
		}
	}
}

// Round é UMA rodada: poll + lote + update + debug. Toda decisão do epic §5
// acontece aqui, nesta ordem.
func (a *Agent) Round(ctx context.Context) RoundResult {
	cfgPolled, jobs, err := a.Client.Poll(ctx)
	if err != nil {
		a.Dbg.Addf("poll falhou: %v", err)
		if StatusOf(err) == 404 {
			// 404 no poll: mesmo tratamento do passo 3 — 30s fixo, sem
			// backoff (epic §4.5); o token.txt pode ter sido trocado.
			// Resposta do servidor zera o backoff (contrato: reset no
			// sucesso) — backoff velho não sobrevive ao episódio de 404.
			a.Backoff = 0
			a.logNoise("poll404", "poll: 404 — token rejeitado ou comandas desligadas; retry fixo em 30s")
			return RoundResult{Sleep: NotFoundRetry}
		}
		if a.Backoff == 0 {
			a.Backoff = a.baseInterval()
		} else {
			a.Backoff = minDur(a.Backoff*2, maxBackoff)
		}
		a.Dbg.Addf("falha de poll/rede/servidor — backoff armado %s", a.Backoff)
		a.Log.Printf("poll falhou (%v) — backoff %s", err, a.Backoff)
		return RoundResult{Sleep: a.Backoff}
	}
	a.Backoff = 0

	if !cfgPolled.Configured {
		// Admin desfaz a impressora no meio do dia → congela a escrita:
		// nenhum write, nenhum ack; segue pollando (heartbeat).
		a.logNoise("cfgfrozen", "config incompleta (configured=false) — escrita congelada (nenhum write, nenhum ack), segue pollando")
		a.sendDebug(ctx)
		return RoundResult{Sleep: ClampInterval(cfgPolled.PollInterval)}
	}

	// config_version mudou → aplica após ESTA rodada (o lote em curso
	// termina com a config vigente; a nova vale a partir do poll seguinte).
	if a.HaveConfig && cfgPolled.ConfigVersion != a.Cur.ConfigVersion {
		a.Dbg.Addf("config_version mudou (%s → %s) — aplica após esta rodada", a.Cur.ConfigVersion, cfgPolled.ConfigVersion)
		a.PendingConfig = cfgPolled
	} else if !a.HaveConfig {
		a.applyConfig(cfgPolled)
	}

	idle := len(jobs) == 0
	if !idle {
		// Bracket do lote (visibilidade de crash): se o processo morrer no
		// meio do write, o arquivo mostra que os jobs CHEGARAM e onde parou.
		a.Dbg.Addf("poll: %d job(s) recebido(s) — imprimindo na ordem", len(jobs))
		a.lastHeart = a.now() // atividade real; o heartbeat de idle conta daqui
	}
	for _, job := range jobs {
		w := a.WriterFor(a.Cur)
		if werr := w.Write(ctx, a.Cur, job.Payload); werr != nil {
			// Write falhou → quebra o lote, sem ACK (evita N dials ruins):
			// job segue pending, badge sobe, agente segue online (epic §5).
			a.printFailed++
			a.Dbg.Addf("job %d (%s) falhou: %v — lote quebrado, sem ack; job segue pending, próximo poll tenta de novo", job.ID, job.Kind, werr)
			break
		}
		pending, aerr := a.Client.Ack(ctx, job.ID)
		if aerr != nil {
			// ACK falhou (5xx/timeout/4xx) → loga e segue o lote:
			// at-least-once, o job volta no próximo poll e reimprime.
			a.ackLost++
			a.Dbg.Addf("job %d (%s) escrito (%d bytes) mas ack falhou: %v — segue o lote; job reimprime no próximo poll (at-least-once)", job.ID, job.Kind, len(job.Payload), aerr)
			continue
		}
		a.printedOK++
		a.Dbg.Addf("job %d (%s) → %s · %d bytes ok · ack ok (pending %d)", job.ID, job.Kind, describeTarget(a.Cur), len(job.Payload), pending)
	}

	if idle {
		// Heartbeat local: rodada idle não loga linha nenhuma — silêncio no
		// arquivo não distingue "vivo esperando job" de "loop morto". A 1ª
		// rodada idle loga na hora (lastHeart vem atrasado do boot) e as
		// seguintes a cada heartbeatGap, com os números da sessão.
		if n := a.now(); n.Sub(a.lastHeart) >= heartbeatGap {
			a.lastHeart = n
			a.Dbg.Addf("poll ok · sem jobs (heartbeat local) · %s", a.sessionSuffix())
		}
		// Rodada vazia é a janela do auto-update (epic §7) — AQUI vive a
		// regra "update só em rodada idle" (HandleOffer/InstallIfReady nem
		// são chamados em rodada com lote): oferta nova dispara o download;
		// download pronto + oferta inalterada → swap.
		a.Updater.HandleOffer(cfgPolled.Offer)
		if spawned := a.Updater.InstallIfReady(); spawned {
			// Última viagem do processo velho: posta o buffer (com o
			// histórico do update) e encerra. O POST falhado é engolido.
			a.sendDebug(ctx)
			return RoundResult{Exit: true}
		}
	}

	// Aplicação de config nova: só entre rodadas (epic §5).
	if a.PendingConfig != nil {
		a.applyConfig(a.PendingConfig)
	}

	a.sendDebug(ctx)
	return RoundResult{Sleep: ClampInterval(a.Cur.PollInterval)}
}

func (a *Agent) WriterFor(cfg DeviceConfig) Writer {
	if cfg.Connection == "usb" {
		return a.Usb
	}
	return NetworkWriter{}
}

// sendDebug posta o buffer inteiro quando debug_mode (decisão 18): a rodada
// seguinte já sai com o histórico recente se o admin ligar o debug ao vivo.
// Falhou → engole (nunca vira segunda fila). Resposta {"stored": N} é logada.
func (a *Agent) sendDebug(ctx context.Context) {
	if !a.HaveConfig || !a.Cur.DebugMode {
		return
	}
	entries := a.Dbg.Snapshot()
	stored, err := a.Client.SendLogs(ctx, entries)
	if err != nil {
		a.Log.Printf("post de debug logs falhou (engolido — nunca vira segunda fila): %v", err)
		return
	}
	a.Log.Printf("debug logs postados: %d entrada(s), servidor armazenou %d", len(entries), stored)
}
