package tests

import (
	"context"
	"localissimo-print-agent/internal/agent"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ---------- passo 1: token ----------

func TestTokenGateSemTokenFicaVivo(t *testing.T) {
	dir := t.TempDir()
	a := newTestAgent(t, dir, "http://irrelevante", nil)

	var sleeps []time.Duration
	// Sem token.txt e ctx cancelado no primeiro sleep → sai limpo com retry 30s.
	ctx, cancel := context.WithCancel(context.Background())
	a.Sleep = func(d time.Duration) {
		sleeps = append(sleeps, d)
		cancel()
	}
	if a.TokenGate(ctx) {
		t.Fatalf("tokenGate deveria sair com ctx cancelado")
	}
	if len(sleeps) != 1 || sleeps[0] != agent.NoTokenRetry {
		t.Fatalf("esperava 1 sleep de 30s, veio %v", sleeps)
	}

	// Token chega (o arquivo pode chegar depois; o agente nunca sai — epic §3).
	writeTokenFile(t, dir, "tok")
	if !a.TokenGate(context.Background()) {
		t.Fatalf("tokenGate deveria passar com token no arquivo")
	}

	// Token com espaços/quebra de linha → trim.
	if err := os.WriteFile(filepath.Join(dir, agent.TokenFileName), []byte("  tok-x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := agent.ReadToken(a.RC.TokenPath); got != "tok-x" {
		t.Fatalf("trim do token: %q", got)
	}
}

// ---------- passos 2 e 3: report + config gates ----------

func TestBootOrdemReportAntesDeConfig(t *testing.T) {
	dir := t.TempDir()
	writeTokenFile(t, dir, "tok")
	rails := newFakeRails("tok")
	srv := startRails(t, rails)
	rails.setConfig(baseConfig())
	a := newTestAgent(t, dir, srv.URL, rails)

	if !a.ReportGate(context.Background()) {
		t.Fatalf("reportGate falhou")
	}
	if !a.ConfigGate(context.Background()) {
		t.Fatalf("configGate falhou")
	}
	paths := rails.getPaths()
	if len(paths) < 2 || paths[0] != "POST /impressora/printers" || paths[1] != "GET /impressora/config" {
		t.Fatalf("boot fora da ordem (report antes de config): %v", paths)
	}

	// Corpo do report: telemetria + printers array (epic §4 + §7.1).
	reps := rails.getReports()
	if len(reps) != 1 {
		t.Fatalf("esperava 1 report, veio %d", len(reps))
	}
	raw := reps[0]["raw"].(string)
	for _, key := range []string{"agent_version", "os", "arch", "hostname", "printers"} {
		if !strings.Contains(raw, `"`+key+`"`) {
			t.Errorf("report sem chave %q: %s", key, raw)
		}
	}
	if reps[0]["agent_version"] != agent.Version {
		t.Errorf("report deve carregar agent_version=%q, veio %q", agent.Version, reps[0]["agent_version"])
	}

	// configGate aplicou a config embutida do fake.
	if !a.HaveConfig || !a.Cur.Configured || a.Cur.PollInterval != 3 {
		t.Fatalf("config não aplicada: %+v", a.Cur)
	}
}

func TestReportGateBackoffE404(t *testing.T) {
	dir := t.TempDir()
	writeTokenFile(t, dir, "tok-velho")
	rails := newFakeRails("tok")
	srv := startRails(t, rails)
	a := newTestAgent(t, dir, srv.URL, rails)

	// 404 (token trocado no servidor): retry fixo de 30s, sem backoff
	// crescendo — o agente volta sozinho quando o token for corrigido.
	var sleeps []time.Duration
	ctx, cancel := context.WithCancel(context.Background())
	a.Sleep = func(d time.Duration) { sleeps = append(sleeps, d); cancel() }
	if a.ReportGate(ctx) {
		t.Fatalf("reportGate deveria sair com ctx cancelado")
	}
	if len(sleeps) != 1 || sleeps[0] != agent.NotFoundRetry {
		t.Fatalf("404 no report: esperava 1 sleep fixo 30s, veio %v", sleeps)
	}

	// Token consertado + falha de rede/5xx: backoff exponencial
	// 3s → 6s → 12s (base = default 3s, teto 60s).
	writeTokenFile(t, dir, "tok")
	rails.reportStatus = 500
	a2 := newTestAgent(t, dir, srv.URL, rails)
	var seq []time.Duration
	ctx2, cancel2 := context.WithCancel(context.Background())
	attempt := 0
	a2.Sleep = func(d time.Duration) {
		seq = append(seq, d)
		attempt++
		if attempt >= 3 {
			cancel2()
		}
	}
	if a2.ReportGate(ctx2) {
		t.Fatalf("reportGate deveria sair com ctx cancelado")
	}
	if len(seq) != 3 || seq[0] != 3*time.Second || seq[1] != 6*time.Second || seq[2] != 12*time.Second {
		t.Fatalf("backoff do report errado: %v", seq)
	}
}

func TestConfigGateConfiguredFalseFicaConsultando(t *testing.T) {
	dir := t.TempDir()
	writeTokenFile(t, dir, "tok")
	rails := newFakeRails("tok")
	srv := startRails(t, rails)
	rails.setConfig(map[string]any{"configured": false, "config_version": nil, "poll_interval": 5})
	a := newTestAgent(t, dir, srv.URL, rails)

	// configured=false consulta no intervalo; na 2ª espera o admin completa
	// a impressora → a 3ª consulta vem completa e o gate passa sem restart.
	polls := 0
	a.Sleep = func(d time.Duration) {
		polls++
		if polls == 2 {
			rails.setConfig(baseConfig())
		}
	}
	if !a.ConfigGate(context.Background()) {
		t.Fatalf("configGate deveria passar quando configured virou true")
	}
	if polls != 2 {
		t.Fatalf("esperava 2 esperas de configured=false, veio %d", polls)
	}
	if !a.HaveConfig || a.Cur.PollInterval != 3 {
		t.Fatalf("config completa não aplicada: %+v", a.Cur)
	}
}

// ---------- passo 4: loop/round ----------

// applyCfg marca a config como vigente no agente (como se o boot tivesse
// passado) apontando para o fake printer.
func applyCfg(a *agent.Agent, fp *fakePrinter, interval int, debug bool, cfgVersion string) {
	cfg := baseConfig()
	h, p := fp.hostPort()
	cfg["host"] = h
	cfg["port"] = p
	cfg["poll_interval"] = interval
	cfg["debug_mode"] = debug
	cfg["config_version"] = cfgVersion
	a.Cur = *parseConfigForTest(cfg)
	a.HaveConfig = true
}

func payloadComanda(marcador string) []byte {
	return append([]byte{0x1b, 0x40}, []byte("COMANDA "+marcador+"\n")...)
}

func setRailsCfg(rails *fakeRails, fp *fakePrinter, extras map[string]any) {
	cfg := baseConfig()
	h, p := fp.hostPort()
	cfg["host"] = h
	cfg["port"] = p
	for k, v := range extras {
		cfg[k] = v
	}
	rails.setConfig(cfg)
}

// Rodada feliz: 2 jobs, ordem preservada, writes completos, acks na ordem,
// dorme o intervalo da config. Coração do epic §5.
func TestRoundImprimeEACKNaOrdem(t *testing.T) {
	dir := t.TempDir()
	writeTokenFile(t, dir, "tok")
	rails := newFakeRails("tok")
	srv := startRails(t, rails)
	a := newTestAgent(t, dir, srv.URL, rails)
	fp := startFakePrinter(t)
	applyCfg(a, fp, 3, false, "v1")
	setRailsCfg(rails, fp, nil)

	p1 := payloadComanda("1")
	p2 := payloadComanda("2")
	rails.setJobs([]map[string]any{jobJSON(101, "kitchen", p1), jobJSON(102, "delivery", p2)})

	res := a.Round(context.Background())
	if res.Exit {
		t.Fatalf("rodada normal não pode pedir exit")
	}
	if res.Sleep != 3*time.Second {
		t.Fatalf("sleep: quero intervalo 3s, veio %v", res.Sleep)
	}
	// Bytes na impressora, na ordem, verbatim (cano burro — nada de
	// transformação no payload).
	got := fp.waitBytes(t, len(p1)+len(p2))
	if string(got) != string(p1)+string(p2) {
		t.Fatalf("impressora recebeu %q; quero %q", got, string(p1)+string(p2))
	}
	// Acks na ordem do servidor (decisão 13) e só depois do write.
	if acked := rails.ackedJobs(); len(acked) != 2 || acked[0] != 101 || acked[1] != 102 {
		t.Fatalf("acks errados: %v", acked)
	}
	if a.Backoff != 0 {
		t.Fatalf("backoff deveria estar zerado: %v", a.Backoff)
	}
}

// Impressora morta: write falha → SEM ack, quebra o lote, mas o poll segue
// fresco (heartbeat — decisão 9; epic §5).
func TestRoundWriteFalhoQuebraLoteSemACK(t *testing.T) {
	dir := t.TempDir()
	writeTokenFile(t, dir, "tok")
	rails := newFakeRails("tok")
	srv := startRails(t, rails)
	a := newTestAgent(t, dir, srv.URL, rails)
	fp := startFakePrinter(t)
	h, p := fp.hostPort()
	fp.Close() // impressora desligada: conexão recusada
	applyCfg(a, fp, 3, false, "v1")

	cfg := baseConfig()
	cfg["host"], cfg["port"] = h, p
	rails.setConfig(cfg)
	rails.setJobs([]map[string]any{
		jobJSON(201, "kitchen", payloadComanda("a")),
		jobJSON(202, "kitchen", payloadComanda("b")),
	})

	res := a.Round(context.Background())
	if res.Sleep != 3*time.Second {
		t.Fatalf("falha de impressão NÃO afasta o poll: sleep %v", res.Sleep)
	}
	if a.Backoff != 0 {
		t.Fatalf("backoff não pode armar por falha de impressora: %v", a.Backoff)
	}
	if acked := rails.ackedJobs(); len(acked) != 0 {
		t.Fatalf("write falho não pode ackar: %v", acked)
	}
	// Entradas de debug citam a falha — mas nunca o payload.
	for _, round := range rails.getLogs() {
		for _, e := range round {
			if strings.Contains(e, "COMANDA") {
				t.Errorf("entrada de debug vazou payload: %s", e)
			}
		}
	}
}

// ACK falhando (5xx) → loga e SEGUE o lote (at-least-once, epic §5).
func TestRoundAckFalhoSegueOLote(t *testing.T) {
	dir := t.TempDir()
	writeTokenFile(t, dir, "tok")
	rails := newFakeRails("tok")
	srv := startRails(t, rails)
	a := newTestAgent(t, dir, srv.URL, rails)
	fp := startFakePrinter(t)
	applyCfg(a, fp, 3, false, "v1")
	setRailsCfg(rails, fp, nil)

	p1 := payloadComanda("1")
	p2 := payloadComanda("2")
	rails.setJobs([]map[string]any{jobJSON(301, "kitchen", p1), jobJSON(302, "kitchen", p2)})
	rails.ackStatus = 500 // ack do servidor quebrado — o agente não pode travar

	res := a.Round(context.Background())
	if res.Exit {
		t.Fatal("nada de exit aqui")
	}
	// Os DOIS writes aconteceram — o lote inteiro é percorrido apesar do ack ruim.
	got := fp.waitBytes(t, len(p1)+len(p2))
	if string(got) != string(p1)+string(p2) {
		t.Fatalf("lote deveria seguir após ack falho; impressora veio %q", got)
	}
	if acked := rails.ackedJobs(); len(acked) != 0 {
		t.Fatalf("acks não deveriam ter sido aceitos (500): %v", acked)
	}
}

// Poll 404 → retry fixo 30s (sem backoff exponencial — epic §5).
func TestRoundPoll404RetryFixo(t *testing.T) {
	dir := t.TempDir()
	writeTokenFile(t, dir, "tok")
	rails := newFakeRails("tok")
	srv := startRails(t, rails)
	a := newTestAgent(t, dir, srv.URL, rails)
	fp := startFakePrinter(t)
	applyCfg(a, fp, 3, false, "v1")

	rails.pollStatus = 404
	res := a.Round(context.Background())
	if res.Sleep != agent.NotFoundRetry {
		t.Fatalf("404: quero 30s fixo, veio %v", res.Sleep)
	}
	if a.Backoff != 0 {
		t.Fatalf("404 não arma backoff: %v", a.Backoff)
	}
}

// Poll 5xx → backoff exponencial (base = intervalo da config), teto 60s,
// reset no sucesso.
func TestRoundPoll5xxBackoffExponencial(t *testing.T) {
	dir := t.TempDir()
	writeTokenFile(t, dir, "tok")
	rails := newFakeRails("tok")
	srv := startRails(t, rails)
	a := newTestAgent(t, dir, srv.URL, rails)
	fp := startFakePrinter(t)
	applyCfg(a, fp, 2, false, "v1") // intervalo 2s → base do backoff
	// Config do servidor idêntica à vigente (versão igual) — nenhum refetch
	// no meio do teste de backoff.
	setRailsCfg(rails, fp, map[string]any{"poll_interval": 2, "config_version": "v1"})

	rails.pollStatus = 502
	if r := a.Round(context.Background()); r.Sleep != 2*time.Second {
		t.Fatalf("1ª falha: backoff = intervalo (2s), veio %v", r.Sleep)
	}
	if r := a.Round(context.Background()); r.Sleep != 4*time.Second {
		t.Fatalf("2ª falha: backoff dobra (4s), veio %v", r.Sleep)
	}
	a.Backoff = 48 * time.Second
	if r := a.Round(context.Background()); r.Sleep != 60*time.Second {
		t.Fatalf("backoff teto 60s, veio %v", r.Sleep)
	}

	// Sucesso reseta o backoff e volta a dormir o intervalo.
	rails.pollStatus = 200
	rails.setJobs(nil)
	if r := a.Round(context.Background()); a.Backoff != 0 || r.Sleep != 2*time.Second {
		t.Fatalf("sucesso deve zerar backoff e dormir o intervalo: backoff=%v sleep=%v", a.Backoff, r.Sleep)
	}
}

// configured=false no meio do dia: congela a escrita (nenhum write, nenhum
// ack), segue pollando com heartbeat (epic §5).
func TestRoundConfiguredFalseCongelaEscrita(t *testing.T) {
	dir := t.TempDir()
	writeTokenFile(t, dir, "tok")
	rails := newFakeRails("tok")
	srv := startRails(t, rails)
	a := newTestAgent(t, dir, srv.URL, rails)
	fp := startFakePrinter(t)
	applyCfg(a, fp, 3, false, "v1")

	rails.setConfig(map[string]any{"configured": false, "config_version": nil, "poll_interval": 3})
	rails.setJobs([]map[string]any{jobJSON(401, "kitchen", payloadComanda("x"))})

	res := a.Round(context.Background())
	if res.Sleep != 3*time.Second {
		t.Fatalf("sleep em rodada congelada: %v", res.Sleep)
	}
	if acked := rails.ackedJobs(); len(acked) != 0 {
		t.Fatalf("configured=false não pode ackar: %v", acked)
	}
	select {
	case b := <-fp.received:
		t.Fatalf("configured=false não pode escrever na impressora; veio %q", b)
	case <-time.After(200 * time.Millisecond):
	}
}

// config_version mudou → aplica só entre rodadas: o lote da rodada usa a
// config vigente; a nova vale no poll seguinte (epic §5).
func TestRoundConfigNovaAplicaEntreRodadas(t *testing.T) {
	dir := t.TempDir()
	writeTokenFile(t, dir, "tok")
	rails := newFakeRails("tok")
	srv := startRails(t, rails)
	a := newTestAgent(t, dir, srv.URL, rails)
	fpOld := startFakePrinter(t) // impressora velha (host v1) — desligada
	fpNew := startFakePrinter(t) // dono trocou a impressora no painel

	// Boot passou com v1 (host velho) vigente no agente.
	applyCfg(a, fpOld, 3, false, "v1")

	h2, p2 := fpNew.hostPort()
	cfgV2 := baseConfig()
	cfgV2["host"], cfgV2["port"] = h2, p2
	cfgV2["config_version"] = "v2"

	// Rodada 1: o servidor já anuncia v2 (host novo) + traz job — o write
	// DEVE usar v1 (host velho, falha) e a config nova fica pendente.
	rails.setConfig(cfgV2)
	rails.setJobs([]map[string]any{jobJSON(501, "kitchen", payloadComanda("n"))})

	fpOld.Close()
	a.Round(context.Background())
	// O write da rodada 1 usou a config VIGENTE (host velho → falhou, sem
	// ack) — a config nova não vale no meio do lote...
	if acked := rails.ackedJobs(); len(acked) != 0 {
		t.Fatalf("write falho (host velho) não acka: %v", acked)
	}
	// ...e a aplicação aconteceu entre rodadas: a rodada acabou, v2 vigente.
	if a.Cur.ConfigVersion != "v2" {
		t.Fatalf("config v2 deveria estar vigente após a rodada 1 (aplicação entre rodadas): %+v", a.Cur)
	}
	if a.PendingConfig != nil {
		t.Fatalf("pendente deveria ter sido consumido: %+v", a.PendingConfig)
	}

	// Rodada 2: config nova vigente → write no host novo, ack.
	rails.setJobs([]map[string]any{jobJSON(502, "kitchen", payloadComanda("n2"))})
	a.Round(context.Background())
	if a.Cur.ConfigVersion != "v2" {
		t.Fatalf("config v2 deveria estar vigente: %+v", a.Cur)
	}
	got := fpNew.waitBytes(t, len("COMANDA n2\n")+2)
	if !strings.Contains(string(got), "COMANDA n2") {
		t.Fatalf("job da rodada 2 deveria sair na impressora nova; veio %q", got)
	}
	if acked := rails.ackedJobs(); len(acked) != 1 || acked[0] != 502 {
		t.Fatalf("acks rodada 2: %v", acked)
	}
}

// poll_interval ausente/≤0 → default 3s; clamp [1, 60] (epic §5).
func TestClampInterval(t *testing.T) {
	cases := []struct {
		in   int
		want time.Duration
	}{
		{0, 3 * time.Second}, {-5, 3 * time.Second}, {3, 3 * time.Second},
		{1, 1 * time.Second}, {60, 60 * time.Second}, {600, 60 * time.Second},
		{-1, 3 * time.Second},
	}
	for _, c := range cases {
		if got := agent.ClampInterval(c.in); got != c.want {
			t.Errorf("agent.ClampInterval(%d) = %v; quero %v", c.in, got, c.want)
		}
	}
}

// debug_mode ligado: um POST de logs POR RODADA (não por job) com o buffer
// inteiro; falha do POST é engolida (nunca vira segunda fila — epic §6).
func TestDebugModeUmPostPorRodada(t *testing.T) {
	dir := t.TempDir()
	writeTokenFile(t, dir, "tok")
	rails := newFakeRails("tok")
	srv := startRails(t, rails)
	a := newTestAgent(t, dir, srv.URL, rails)
	fp := startFakePrinter(t)
	applyCfg(a, fp, 3, true, "v1")
	// Servidor e agente na MESMA config_version — nenhuma aplicação lateral
	// no meio do teste.
	setRailsCfg(rails, fp, map[string]any{"debug_mode": true, "config_version": "v1"})

	pay := payloadComanda("dbg")
	rails.setJobs([]map[string]any{jobJSON(601, "kitchen", pay)})

	a.Round(context.Background())
	logs := rails.getLogs()
	if len(logs) != 1 {
		t.Fatalf("1 rodada = 1 POST de logs; veio %d", len(logs))
	}
	joined := strings.Join(logs[0], "\n")
	if !strings.Contains(joined, "601") || !strings.Contains(joined, "kitchen") {
		t.Fatalf("entradas devem citar id/kind do job: %s", joined)
	}
	if strings.Contains(joined, "COMANDA dbg") {
		t.Fatalf("entrada de debug NUNCA contém payload: %s", joined)
	}

	// Segunda rodada (vazia): outro POST, agora com histórico (buffer inteiro).
	rails.setJobs(nil)
	a.Round(context.Background())
	logs = rails.getLogs()
	if len(logs) != 2 {
		t.Fatalf("esperava 2 POSTs de logs, veio %d", len(logs))
	}
	if len(logs[1]) < len(logs[0]) {
		t.Fatalf("buffer inteiro deve ser postado (histórico incluído)")
	}

	// POST falhando (5xx) → engolido, a rodada segue normal.
	rails.logsStatus = 500
	rails.setJobs([]map[string]any{jobJSON(602, "kitchen", pay)})
	if res := a.Round(context.Background()); res.Sleep != 3*time.Second {
		t.Fatalf("falha do post de logs não muda a cadência: %v", res.Sleep)
	}

	// debug_mode=false (admin desligou ao vivo) → nenhum POST.
	rails.logsStatus = 200
	applyCfg(a, fp, 3, false, "v1")
	setRailsCfg(rails, fp, map[string]any{"debug_mode": false, "config_version": "v1"})
	before := len(rails.getLogs())
	a.Round(context.Background())
	if len(rails.getLogs()) != before {
		t.Fatalf("debug off não posta nada")
	}
}

// rodada vazia (idle): poll segue como heartbeat e dorme o intervalo.
func TestRoundVaziaHeartbeat(t *testing.T) {
	dir := t.TempDir()
	writeTokenFile(t, dir, "tok")
	rails := newFakeRails("tok")
	srv := startRails(t, rails)
	a := newTestAgent(t, dir, srv.URL, rails)
	fp := startFakePrinter(t)
	applyCfg(a, fp, 7, false, "v1")
	setRailsCfg(rails, fp, map[string]any{"poll_interval": 7})
	rails.setJobs(nil)

	res := a.Round(context.Background())
	if res.Sleep != 7*time.Second || res.Exit {
		t.Fatalf("rodada vazia: dorme intervalo (7s), veio %+v", res)
	}
}

// Run de ponta a ponta (boot 4 passos + loop), com sleeper instantâneo e
// cancelamento controlado pelo fake Rails: report → config → 1º poll com job
// → write → ack → próxima rodada vazia → cancel. É o teste que garante que
// as peças engrenam na ordem do epic §4.
func TestRunPontaAPonta(t *testing.T) {
	dir := t.TempDir()
	writeTokenFile(t, dir, "tok")
	rails := newFakeRails("tok")
	srv := startRails(t, rails)
	a := newTestAgent(t, dir, srv.URL, rails)
	fp := startFakePrinter(t)

	cfgSrv := baseConfig()
	h, p := fp.hostPort()
	cfgSrv["host"], cfgSrv["port"] = h, p
	rails.setConfig(cfgSrv)

	pay := payloadComanda("e2e")
	rails.setJobs([]map[string]any{jobJSON(701, "kitchen", pay)})

	ctx, cancel := context.WithCancel(context.Background())
	rounds := 0
	a.Sleep = func(d time.Duration) {
		rounds++
		if rounds == 1 {
			// 1ª rodada imprimiu e ackou; a 2ª rodada roda vazia e encerra.
			if acked := rails.ackedJobs(); len(acked) != 1 || acked[0] != 701 {
				t.Errorf("ponta a ponta: acks após 1ª rodada = %v", acked)
			}
			rails.setJobs(nil)
		}
		if rounds >= 2 {
			cancel()
		}
	}
	a.Run(ctx)

	got := fp.waitBytes(t, len(pay))
	if !strings.Contains(string(got), "COMANDA e2e") {
		t.Fatalf("payload não chegou na impressora: %q", got)
	}
	paths := rails.getPaths()
	if len(paths) < 4 || paths[0] != "POST /impressora/printers" || paths[1] != "GET /impressora/config" {
		t.Fatalf("boot fora da ordem: %v", paths)
	}
	if acked := rails.ackedJobs(); len(acked) != 1 || acked[0] != 701 {
		t.Fatalf("acks finais: %v", acked)
	}
}

// Dispatch de writer por connection (epic §5): usb → agent.USBWriter, network →
// agent.NetworkWriter; guarda do usb sem printer_name.
func TestWriterDispatch(t *testing.T) {
	a := &agent.Agent{Usb: agent.USBWriter{}}
	if _, ok := a.WriterFor(agent.DeviceConfig{Connection: "usb"}).(agent.USBWriter); !ok {
		t.Fatalf("usb deveria resolver agent.USBWriter")
	}
	if _, ok := a.WriterFor(agent.DeviceConfig{Connection: "network"}).(agent.NetworkWriter); !ok {
		t.Fatalf("network deveria resolver agent.NetworkWriter")
	}
	// connection estranha/ausente cai no network (que falha claro se o host
	// estiver vazio — freeze seguro).
	if _, ok := a.WriterFor(agent.DeviceConfig{Connection: ""}).(agent.NetworkWriter); !ok {
		t.Fatalf("connection vazia deveria resolver agent.NetworkWriter")
	}
	if err := (agent.USBWriter{}).Write(context.Background(), agent.DeviceConfig{Connection: "usb", PrinterName: ""}, []byte("x")); err == nil ||
		!strings.Contains(err.Error(), "printer_name") {
		t.Fatalf("usb sem printer_name: %v", err)
	}
}
