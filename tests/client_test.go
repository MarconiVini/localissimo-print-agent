package tests

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"localissimo-print-agent/internal/agent"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// O header de auth segue o contrato em TODAS as rotas (Bearer + token) e
// X-Agent-Version viaja em toda request autenticada (telemetria, epic §7.1).
func TestClientHeadersOnAllRoutes(t *testing.T) {
	rails := newFakeRails("tok-123")
	srv := startRails(t, rails)
	dir := t.TempDir()
	writeTokenFile(t, dir, "tok-123")
	a := newTestAgent(t, dir, srv.URL, rails)
	ctx := context.Background()

	_, err := a.Client.GetConfig(ctx)
	if err != nil {
		t.Fatalf("GetConfig: %v", err)
	}
	_, _, err = a.Client.Poll(ctx)
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}
	_, err = a.Client.Ack(ctx, 7)
	if err != nil {
		t.Fatalf("Ack: %v", err)
	}
	_, err = a.Client.ReportPrinters(ctx, agent.BuildReportBody(nil))
	if err != nil {
		t.Fatalf("ReportPrinters: %v", err)
	}
	_, err = a.Client.SendLogs(ctx, []string{"x"})
	if err != nil {
		t.Fatalf("SendLogs: %v", err)
	}

	n := len(rails.getPaths())
	if n != 5 {
		t.Fatalf("esperava 5 requests, foram %d", n)
	}
	for i := 0; i < n; i++ {
		if got := rails.seenAuth[i]; got != "Bearer tok-123" {
			t.Errorf("request %d: Authorization = %q; quero %q", i, got, "Bearer tok-123")
		}
		if got := rails.seenVersions[i]; got != agent.Version {
			t.Errorf("request %d: X-Agent-Version = %q; quero %q", i, got, agent.Version)
		}
	}

	// Esquema case-insensitive (RFC 7235, mesma razão do Rails): "bearer"
	// minúsculo NÃO pode virar 404.
	clientLower := agent.NewClient(srv.URL, func() string { return "tok-123" })
	clientLower.API.Transport = roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		r2 := r.Clone(r.Context())
		r2.Header.Set("Authorization", "bearer tok-123")
		return http.DefaultTransport.RoundTrip(r2)
	})
	if _, err := clientLower.GetConfig(ctx); err != nil {
		t.Fatalf("esquema bearer minúsculo foi rejeitado: %v", err)
	}
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// Config: campos consumidos pelo agente; null → zeros; agent_* só formam
// oferta quando os três vieram.
func TestClientGetConfigParsing(t *testing.T) {
	rails := newFakeRails("tok")
	srv := startRails(t, rails)
	dir := t.TempDir()
	writeTokenFile(t, dir, "tok")
	a := newTestAgent(t, dir, srv.URL, rails)
	ctx := context.Background()

	rails.setConfig(map[string]any{
		"configured": true, "config_version": "2026-11-16T10:00:00-03:00",
		"connection": "Network", "host": "192.168.0.50", "port": 9100,
		"printer_name": "POS-80", "poll_interval": 7, "debug_mode": true,
		"agent_version": "1.2.3", "agent_download_url": "https://x/y", "agent_sha256": "ABCD",
	})
	cfg, err := a.Client.GetConfig(ctx)
	if err != nil {
		t.Fatalf("GetConfig: %v", err)
	}
	if !cfg.Configured || cfg.Connection != "network" || cfg.Host != "192.168.0.50" ||
		cfg.Port != 9100 || cfg.PrinterName != "POS-80" || cfg.PollInterval != 7 || !cfg.DebugMode {
		t.Fatalf("config parseada errada: %+v", cfg)
	}
	if cfg.Offer == nil || cfg.Offer.Version != "1.2.3" || cfg.Offer.URL != "https://x/y" || cfg.Offer.SHA256 != "abcd" {
		t.Fatalf("oferta errada: %+v", cfg.Offer)
	}

	// Nulls / ausentes → config zerada, sem oferta.
	rails.setConfig(map[string]any{
		"configured": false, "config_version": nil, "connection": nil,
		"host": nil, "port": nil, "printer_name": nil, "poll_interval": nil,
		"debug_mode": nil, "agent_version": nil, "agent_download_url": "https://x",
	})
	cfg, err = a.Client.GetConfig(ctx)
	if err != nil {
		t.Fatalf("GetConfig null: %v", err)
	}
	if cfg.Configured || cfg.ConfigVersion != "" || cfg.Connection != "" || cfg.Host != "" ||
		cfg.Port != 0 || cfg.PollInterval != 0 || cfg.DebugMode {
		t.Fatalf("config com null deveria zerar tudo: %+v", cfg)
	}
	if cfg.Offer != nil {
		t.Fatalf("oferta parcial (sem version/sha) não pode virar oferta: %+v", cfg.Offer)
	}
}

// Poll: base64 std com padding decodifica; ordem do servidor preservada
// (decisão 13); payload corrompido é erro de rodada (sem write, sem ack).
func TestClientPollParsing(t *testing.T) {
	rails := newFakeRails("tok")
	srv := startRails(t, rails)
	dir := t.TempDir()
	writeTokenFile(t, dir, "tok")
	a := newTestAgent(t, dir, srv.URL, rails)
	ctx := context.Background()

	p1 := []byte{0x1b, 0x40, 0x1d, 0x56, 0x00}
	p2 := []byte{0x1b, 0x61, 0x01, 'a', 'b'}
	rails.setConfig(baseConfig())
	rails.setJobs([]map[string]any{jobJSON(9, "kitchen", p1), jobJSON(11, "delivery", p2)})

	_, jobs, err := a.Client.Poll(ctx)
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if len(jobs) != 2 {
		t.Fatalf("esperava 2 jobs: %+v", jobs)
	}
	if jobs[0].ID != 9 || jobs[0].Kind != "kitchen" || string(jobs[0].Payload) != string(p1) {
		t.Fatalf("job 0 errado: %+v", jobs[0])
	}
	if jobs[1].ID != 11 || jobs[1].Kind != "delivery" || string(jobs[1].Payload) != string(p2) {
		t.Fatalf("job 1 errado: %+v", jobs[1])
	}

	// Ordem do servidor preservada: inverter no servidor, inverter no cliente.
	rails.setJobs([]map[string]any{jobJSON(11, "delivery", p2), jobJSON(9, "kitchen", p1)})
	_, jobs, _ = a.Client.Poll(ctx)
	if jobs[0].ID != 11 || jobs[1].ID != 9 {
		t.Fatalf("agente reordenou o lote (nunca reordenar!): %+v", jobs)
	}

	// Base64 inválido → erro do poll (o agente não escreve nem acka).
	rails.setJobs([]map[string]any{{"id": float64(5), "kind": "kitchen", "payload": "não-base64!!!"}})
	if _, _, err := a.Client.Poll(ctx); err == nil {
		t.Fatalf("payload base64 inválido deveria dar erro")
	}
}

func TestClientAckAndStoredParsing(t *testing.T) {
	rails := newFakeRails("tok")
	srv := startRails(t, rails)
	dir := t.TempDir()
	writeTokenFile(t, dir, "tok")
	a := newTestAgent(t, dir, srv.URL, rails)
	ctx := context.Background()

	pending, err := a.Client.Ack(ctx, 42)
	if err != nil {
		t.Fatalf("Ack: %v", err)
	}
	if pending != 0 {
		t.Fatalf("pending: quero 0 (fila vazia), veio %d", pending)
	}
	rails.setJobs([]map[string]any{jobJSON(1, "kitchen", []byte("x")), jobJSON(2, "kitchen", []byte("y"))})
	if pending, _ := a.Client.Ack(ctx, 1); pending != 2 {
		t.Fatalf("pending após 1 job na fila: quero 2, veio %d", pending)
	}
	paths := rails.getPaths()
	if paths[len(paths)-1] != "POST /impressora/jobs/1/ack" {
		t.Fatalf("path do ack: %q", paths[len(paths)-1])
	}

	if stored, err := a.Client.ReportPrinters(ctx, agent.BuildReportBody(nil)); err != nil || stored != 0 {
		t.Fatalf("ReportPrinters vazio: stored=%d err=%v", stored, err)
	}
	if stored, err := a.Client.SendLogs(ctx, []string{"a", "b"}); err != nil || stored != 2 {
		t.Fatalf("SendLogs: stored=%d err=%v", stored, err)
	}
}

// 404 indistinto é classificável: agent.HTTPError com Status 404 → retry fixo.
func TestClientHTTPErrorClassification(t *testing.T) {
	rails := newFakeRails("tok-certo")
	srv := startRails(t, rails)
	dir := t.TempDir()
	writeTokenFile(t, dir, "tok-errado")
	a := newTestAgent(t, dir, srv.URL, rails)
	ctx := context.Background()

	_, err := a.Client.GetConfig(ctx)
	he, ok := err.(*agent.HTTPError)
	if !ok || he.Status != 404 {
		t.Fatalf("token errado deveria dar agent.HTTPError 404, veio %v", err)
	}

	// Token consertado + servidor com problema real: 5xx classificável.
	writeTokenFile(t, dir, "tok-certo")
	rails.pollStatus = 500
	_, _, err = a.Client.Poll(ctx)
	he, ok = err.(*agent.HTTPError)
	if !ok || he.Status != 500 {
		t.Fatalf("5xx deveria dar agent.HTTPError 500, veio %v", err)
	}
	rails.pollStatus = 200

	// Servidor fora do ar → erro de rede (agent.StatusOf == 0).
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	deadURL := dead.URL
	dead.Close()
	dead2 := agent.NewClient(deadURL, func() string { return "t" })
	if _, _, err := dead2.Poll(ctx); err == nil || agent.StatusOf(err) != 0 {
		t.Fatalf("servidor morto deveria ser falha de rede (status 0), veio %v", err)
	}
}

// Download: sha256 calculado em voo bate; Bearer no header; guarda https
// (epic §7.2) na URL inicial e em cada salto de redirect.
func TestClientDownload(t *testing.T) {
	dir := t.TempDir()
	content := []byte("binário-fake-do-release-0.1.0")
	rails := newFakeRails("tok")
	srv := startRails(t, rails)
	client := agent.NewClient(srv.URL, func() string { return "tok" })
	// DownloadToFile não exige a rota /impressora — o handler cai no default
	// 404; para o teste servimos o binário num servidor próprio com a mesma
	// auth (url do release é rota própria/hospedagem externa no mundo real).
	dlSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/redirect") {
			http.Redirect(w, r, "http://evil.example/bin", http.StatusFound)
			return
		}
		_, _ = w.Write(content)
	}))
	defer dlSrv.Close()

	dst := filepath.Join(dir, "baixado")
	sha, err := client.DownloadToFile(context.Background(), dlSrv.URL+"/bin", dst)
	if err != nil {
		t.Fatalf("DownloadToFile: %v", err)
	}
	sum := sha256.Sum256(content)
	if sha != hex.EncodeToString(sum[:]) {
		t.Fatalf("sha diverge: %s", sha)
	}
	got, err := os.ReadFile(dst)
	if err != nil || string(got) != string(content) {
		t.Fatalf("conteúdo baixado errado: %v %q", err, got)
	}
	st, _ := os.Stat(dst)
	if st.Mode().Perm() != 0o755 {
		t.Fatalf("download deve nascer 0755 (epic §3), veio %v", st.Mode().Perm())
	}

	// Base https: download http é recusado de cara (produção).
	httpsClient := agent.NewClient("https://localissimo.com.br", func() string { return "tok" })
	if _, err := httpsClient.DownloadToFile(context.Background(), dlSrv.URL+"/bin", dst); err == nil ||
		!strings.Contains(err.Error(), "https") {
		t.Fatalf("download http com base https deveria ser recusado: %v", err)
	}

	// Redirect para não-https é bloqueado (redirect cross-host derruba o
	// header Authorization — url externa deve vir pré-assinada).
	tlsSrv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, dlSrv.URL+"/bin", http.StatusFound)
	}))
	defer tlsSrv.Close()
	secClient := agent.NewClient(tlsSrv.URL, func() string { return "tok" })
	secClient.HTTPSOnly = true
	// Confiar no cert de teste (não é "desligar verificação" — é o root do
	// httptest; produção nunca toca aqui).
	pool := x509.NewCertPool()
	pool.AddCert(tlsSrv.Certificate())
	secClient.DL.Transport = &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}}
	if _, err := secClient.DownloadToFile(context.Background(), tlsSrv.URL+"/bin", dst); err == nil ||
		!strings.Contains(err.Error(), "https") {
		t.Fatalf("redirect para http deveria ser bloqueado: %v", err)
	}

	// Download 404 (token inválido na hospedagem) → agent.HTTPError.
	railsBad := agent.NewClient(srv.URL, func() string { return "token-errado" })
	_, derr := railsBad.DownloadToFile(context.Background(), dlSrv.URL+"/bin", dst)
	if derr == nil {
		t.Fatalf("download com token errado deveria falhar")
	}
	if _, ok := derr.(*agent.HTTPError); !ok {
		t.Fatalf("esperava agent.HTTPError, veio %T (%v)", derr, derr)
	}
}
