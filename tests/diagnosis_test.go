package tests

import (
	"context"
	"localissimo-print-agent/internal/agent"
	"strings"
	"testing"
	"time"
)

// Conta quantas entradas do buffer citam a substring (observável black-box
// do throttle: disparo loga, suprimido não).
func contaNoDebug(a *agent.Agent, sub string) int {
	n := 0
	for _, e := range a.Dbg.Snapshot() {
		if strings.Contains(e, sub) {
			n++
		}
	}
	return n
}

// Estado estático (configured=false no gate) NÃO pode virar uma linha a
// cada 3s — foi 2/3 do agent.log real do Windows. 1ª ocorrência loga, a
// seguinte (dentro do noiseGap) é suprimida; quando o estado passa, o gate
// sai sem linha extra.
func TestRuidoConfiguredFalseSuprimido(t *testing.T) {
	dir := t.TempDir()
	writeTokenFile(t, dir, "tok")
	rails := newFakeRails("tok")
	srv := startRails(t, rails)
	rails.setConfig(map[string]any{"configured": false, "config_version": nil, "poll_interval": 5})
	a := newTestAgent(t, dir, srv.URL, rails)

	polls := 0
	a.Sleep = func(d time.Duration) {
		polls++
		if polls == 2 {
			rails.setConfig(baseConfig()) // admin completa a impressora
		}
	}
	if !a.ConfigGate(context.Background()) {
		t.Fatalf("configGate deveria passar")
	}
	if got := contaNoDebug(a, "configured=false"); got != 1 {
		t.Fatalf("3 consultas com configured=false: 1 linha no debug, veio %d", got)
	}
}

// O throttle acumula as suprimidas: depois do gap, o re-disparo traz o total.
func TestRuidoThrottleRedisparaComContagem(t *testing.T) {
	dir := t.TempDir()
	writeTokenFile(t, dir, "tok")
	rails := newFakeRails("tok")
	srv := startRails(t, rails)
	rails.setConfig(map[string]any{"configured": false, "config_version": nil, "poll_interval": 5})
	a := newTestAgent(t, dir, srv.URL, rails)

	// Relógio controlado: 4 consultas em 30s (1ª loga, 3 suprimidas), o
	// relógio pula 6 min na 4ª espera e a 5ª consulta re-loga com
	// "3 suprimida(s)"; o admin completa a impressora só na 6ª.
	now := time.Unix(1_800_000_000, 0)
	a.Now = func() time.Time { return now }
	polls := 0
	a.Sleep = func(d time.Duration) {
		polls++
		if polls == 4 {
			now = now.Add(6 * time.Minute) // pula o noiseGap
		}
		if polls >= 5 {
			rails.setConfig(baseConfig())
		}
	}
	if !a.ConfigGate(context.Background()) {
		t.Fatalf("configGate deveria passar")
	}
	var linhas []string
	for _, e := range a.Dbg.Snapshot() {
		if strings.Contains(e, "configured=false") {
			linhas = append(linhas, e)
		}
	}
	if len(linhas) != 2 {
		t.Fatalf("esperava 1ª linha + re-disparo, veio %d: %v", len(linhas), linhas)
	}
	if !strings.Contains(linhas[1], "3 ocorrência(s) igual(is) suprimida(s)") {
		t.Fatalf("re-disparo deveria trazer as 3 suprimidas: %q", linhas[1])
	}
}

// Heartbeat de idle: a 1ª rodada vazia loga na hora ("vivo, esperando job"),
// as seguintes só depois do gap — silêncio nunca mais significa "loop morto".
func TestHeartbeatIdleNoDebug(t *testing.T) {
	dir := t.TempDir()
	writeTokenFile(t, dir, "tok")
	rails := newFakeRails("tok")
	srv := startRails(t, rails)
	a := newTestAgent(t, dir, srv.URL, rails)
	fp := startFakePrinter(t)
	applyCfg(a, fp, 3, false, "v1")
	setRailsCfg(rails, fp, nil)
	rails.setJobs(nil)

	now := time.Unix(1_800_000_000, 0)
	a.Now = func() time.Time { return now }

	a.Round(context.Background()) // 1ª rodada idle → heartbeat na hora
	if got := contaNoDebug(a, "heartbeat local"); got != 1 {
		t.Fatalf("1ª rodada idle deveria logar heartbeat, veio %d", got)
	}

	now = now.Add(4 * time.Minute)
	a.Round(context.Background()) // dentro do gap → nada
	if got := contaNoDebug(a, "heartbeat local"); got != 1 {
		t.Fatalf("4min depois do heartbeat não pode re-logar, veio %d", got)
	}

	now = now.Add(2 * time.Minute) // 6min desde o último → re-loga
	a.Round(context.Background())
	if got := contaNoDebug(a, "heartbeat local"); got != 2 {
		t.Fatalf("passado o gap deve re-logar com os números da sessão, veio %d", got)
	}
	for _, e := range a.Dbg.Snapshot() {
		if strings.Contains(e, "heartbeat local") && strings.Contains(e, "sessão ") {
			return // achou o heartbeat com resumo
		}
	}
	t.Fatalf("heartbeat deveria carregar a sessão (sessão Ns · N ok): %v", a.Dbg.Snapshot())
}

// Lote recebido fica marcado ANTES do write (bracket de crash): se o
// processo morre no meio, o log mostra que os jobs chegaram e quantos eram.
func TestLoteMarcadoNoDebug(t *testing.T) {
	dir := t.TempDir()
	writeTokenFile(t, dir, "tok")
	rails := newFakeRails("tok")
	srv := startRails(t, rails)
	a := newTestAgent(t, dir, srv.URL, rails)
	fp := startFakePrinter(t)
	applyCfg(a, fp, 3, false, "v1")
	setRailsCfg(rails, fp, nil)
	rails.setJobs([]map[string]any{
		jobJSON(101, "kitchen", payloadComanda("1")),
		jobJSON(102, "delivery", payloadComanda("2")),
	})

	a.Round(context.Background())
	if got := contaNoDebug(a, "poll: 2 job(s) recebido(s)"); got != 1 {
		t.Fatalf("lote deveria estar marcado no debug: %v", a.Dbg.Snapshot())
	}
}

// Erro HTTP com corpo multilinha (página de exceção do Rails) vira UMA
// linha — sem \n/\t, o agent.log segue 1 evento = 1 linha com timestamp.
func TestHTTPErrorCorpoEmUmaLinha(t *testing.T) {
	e := &agent.HTTPError{Status: 500, Body: "<html>\n<head>\r\n\t<title>Boom</title>\n</head>fim-da-causa"}
	msg := e.Error()
	if strings.ContainsAny(msg, "\n\r\t") {
		t.Fatalf("erro não pode conter quebra/tab: %q", msg)
	}
	if !strings.Contains(msg, "http 500") || !strings.Contains(msg, "fim-da-causa") {
		t.Fatalf("mensagem: %q", msg)
	}
	// O cap de ~300 chars do corpo continua valendo.
	longa := &agent.HTTPError{Status: 500, Body: strings.Repeat("x", 1000)}
	if len(longa.Error()) > 340 {
		t.Fatalf("cap do corpo quebrou: %d chars", len(longa.Error()))
	}
}

// Resumo da sessão no encerramento: quantos jobs a sessão imprimiu fica no
// log (via debug/espelho), não só no painel.
func TestResumoSessaoNoEncerramento(t *testing.T) {
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
	rails.setJobs([]map[string]any{jobJSON(701, "kitchen", payloadComanda("sum"))})

	ctx, cancel := context.WithCancel(context.Background())
	rounds := 0
	a.Sleep = func(d time.Duration) {
		rounds++
		if rounds == 1 {
			rails.setJobs(nil) // ack não tira o job do fake — 2ª rodada vazia
		}
		if rounds >= 2 {
			cancel() // 2ª rodada vazia → encerra com resumo
		}
	}
	a.Run(ctx)

	var resumo string
	for _, e := range a.Dbg.Snapshot() {
		if strings.Contains(e, "encerrando — jobs pendentes") {
			resumo = e
		}
	}
	if resumo == "" {
		t.Fatalf("encerramento deveria logar resumo: %v", a.Dbg.Snapshot())
	}
	if !strings.Contains(resumo, "1 job(s) ok") {
		t.Fatalf("resumo deveria contar o job impresso: %q", resumo)
	}
}
