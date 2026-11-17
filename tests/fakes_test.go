package tests

// Harness de testes sem hardware (epic §12): fake Rails via httptest
// espelhando o PrintDevicesController do S4 (404 indistinto com esquema
// case-insensitive, config embutida no poll, ack idempotente com {pending})
// + fake impressora TCP RAW + construtor de agente de teste.

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"localissimo-print-agent/internal/agent"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---------- fake Rails ----------

type fakeRails struct {
	mu      sync.Mutex
	token   string
	cfg     map[string]any // payload de config (impressora_config_payload)
	jobs    []map[string]any
	acked   []int64
	reports []map[string]any
	logs    [][]string

	configStatus int
	pollStatus   int
	logsStatus   int
	ackStatus    int
	reportStatus int

	seenAuth     []string
	seenVersions []string
	paths        []string
	bodies       []string

	onPoll func() // hook de teste (cancela ctx, conta rodadas...)
}

func newFakeRails(token string) *fakeRails {
	return &fakeRails{
		token:        token,
		configStatus: 200,
		pollStatus:   200,
		logsStatus:   200,
		ackStatus:    200,
		reportStatus: 200,
	}
}

func (f *fakeRails) setConfig(cfg map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cfg = cfg
}

func (f *fakeRails) setJobs(jobs []map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.jobs = jobs
}

func (f *fakeRails) ackedJobs() []int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]int64(nil), f.acked...)
}

func (f *fakeRails) getReports() []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]map[string]any, len(f.reports))
	copy(out, f.reports)
	return out
}

func (f *fakeRails) getLogs() [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([][]string, len(f.logs))
	copy(out, f.logs)
	return out
}

func (f *fakeRails) getPaths() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.paths...)
}

func (f *fakeRails) getBodies() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.bodies...)
}

func (f *fakeRails) lastHeaders() (auth, agentVersion string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.seenAuth) == 0 {
		return "", ""
	}
	return f.seenAuth[len(f.seenAuth)-1], f.seenVersions[len(f.seenVersions)-1]
}

var ackPathRe = regexp.MustCompile(`^/impressora/jobs/(\d+)/ack$`)

func (f *fakeRails) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.paths = append(f.paths, r.Method+" "+r.URL.Path)
	f.seenAuth = append(f.seenAuth, r.Header.Get("Authorization"))
	f.seenVersions = append(f.seenVersions, r.Header.Get("X-Agent-Version"))

	// Autenticação como no Rails: esquema Bearer case-insensitive + token
	// exato; qualquer divergência → 404 indistinto.
	auth := r.Header.Get("Authorization")
	scheme, cred, _ := strings.Cut(auth, " ")
	if !strings.EqualFold(scheme, "bearer") || strings.TrimSpace(cred) != f.token {
		f.mu.Unlock()
		w.WriteHeader(http.StatusNotFound)
		return
	}
	body := ""
	if r.Body != nil {
		raw, _ := io.ReadAll(io.LimitReader(r.Body, 4<<20))
		body = string(raw)
	}
	f.bodies = append(f.bodies, body)
	f.mu.Unlock()

	switch {
	case path_is(r, "/impressora/config", http.MethodGet):
		f.mu.Lock()
		st := f.configStatus
		cfg := f.cfg
		f.mu.Unlock()
		if st != 200 {
			w.WriteHeader(st)
			return
		}
		writeJSON(w, cfg)

	case path_is(r, "/impressora/jobs", http.MethodGet):
		if f.onPoll != nil {
			f.onPoll()
		}
		f.mu.Lock()
		st := f.pollStatus
		cfg := f.cfg
		jobs := f.jobs
		f.mu.Unlock()
		if st != 200 {
			w.WriteHeader(st)
			return
		}
		resp := map[string]any{}
		for k, v := range cfg {
			resp[k] = v
		}
		resp["jobs"] = jobs
		writeJSON(w, resp)

	case path_is(r, "/impressora/printers", http.MethodPost):
		var rep struct {
			Printers     []map[string]any `json:"printers"`
			AgentVersion string           `json:"agent_version"`
		}
		_ = json.Unmarshal([]byte(body), &rep)
		f.mu.Lock()
		st := f.reportStatus
		if st == 200 {
			f.reports = append(f.reports, map[string]any{
				"printers":      rep.Printers,
				"agent_version": rep.AgentVersion,
				"raw":           body,
			})
		}
		f.mu.Unlock()
		if st != 200 {
			w.WriteHeader(st)
			return
		}
		writeJSON(w, map[string]any{"stored": len(rep.Printers)})

	case path_is(r, "/impressora/logs", http.MethodPost):
		var req struct {
			Entries []string `json:"entries"`
		}
		_ = json.Unmarshal([]byte(body), &req)
		f.mu.Lock()
		st := f.logsStatus
		if st == 200 {
			f.logs = append(f.logs, req.Entries)
		}
		f.mu.Unlock()
		if st != 200 {
			w.WriteHeader(st)
			return
		}
		writeJSON(w, map[string]any{"stored": len(req.Entries)})

	default:
		if m := ackPathRe.FindStringSubmatch(r.URL.Path); m != nil && r.Method == http.MethodPost {
			var id int64
			fmt.Sscanf(m[1], "%d", &id)
			f.mu.Lock()
			st := f.ackStatus
			if st == 200 {
				f.acked = append(f.acked, id)
			}
			pending := len(f.jobs)
			f.mu.Unlock()
			if st != 200 {
				w.WriteHeader(st)
				return
			}
			// ACK idempotente (acréscimo Rails do S5): 2xx sempre, com o
			// pending atual — re-ack e job alheio ao mesmo negócio viram no-op.
			writeJSON(w, map[string]any{"pending": pending})
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}
}

// parseConfigForTest passa pela MESMA decodificação do client (a hash do
// fake Rails é a que o servidor de verdade manda).
func parseConfigForTest(cfg map[string]any) *agent.DeviceConfig {
	b, err := json.Marshal(cfg)
	if err != nil {
		panic(err)
	}
	var j agent.ConfigPayload
	if err := json.Unmarshal(b, &j); err != nil {
		panic(err)
	}
	return j.ToConfig()
}

func path_is(r *http.Request, path string, method string) bool {
	return r.URL.Path == path && r.Method == method
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// ---------- payload de config (mesma hash do Rails) ----------

func baseConfig() map[string]any {
	return map[string]any{
		"configured":         true,
		"config_version":     "2026-11-16T10:00:00-03:00",
		"name":               "Cozinha – Tomate MDK-080",
		"connection":         "network",
		"host":               "", // preenchido pelo teste com o endereço do fake printer
		"port":               0,
		"printer_name":       nil,
		"columns":            48,
		"codepage":           "UTF-8",
		"auto_cut":           true,
		"poll_interval":      3,
		"debug_mode":         false,
		"agent_version":      nil,
		"agent_download_url": nil,
		"agent_sha256":       nil,
	}
}

func jobJSON(id int64, kind string, payload []byte) map[string]any {
	return map[string]any{
		"id":      id,
		"kind":    kind,
		"payload": base64.StdEncoding.EncodeToString(payload),
	}
}

// ---------- fake impressora TCP RAW ----------

type fakePrinter struct {
	ln       net.Listener
	received chan []byte
	acceptN  int // quantas conexões ler (0 = todas até o teste acabar)
	closed   bool
}

func startFakePrinter(t *testing.T) *fakePrinter {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("fake printer listen: %v", err)
	}
	fp := &fakePrinter{ln: ln, received: make(chan []byte, 16)}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return // listener fechado
			}
			go func(c net.Conn) {
				c.SetReadDeadline(time.Now().Add(5 * time.Second))
				data, _ := io.ReadAll(c)
				c.Close()
				select {
				case fp.received <- data:
				default:
				}
			}(conn)
		}
	}()
	t.Cleanup(func() { fp.Close() })
	return fp
}

func (fp *fakePrinter) addr() string { return fp.ln.Addr().String() }
func (fp *fakePrinter) hostPort() (string, int) {
	h, p, _ := net.SplitHostPort(fp.addr())
	var pi int
	fmt.Sscanf(p, "%d", &pi)
	return h, pi
}

// Close trava o listener — conexões passam a ser recusadas (impressora morta).
func (fp *fakePrinter) Close() {
	if fp.closed {
		return
	}
	fp.closed = true
	fp.ln.Close()
}

func (fp *fakePrinter) waitBytes(t *testing.T, want int) []byte {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	var got []byte
	for time.Now().Before(deadline) && len(got) < want {
		select {
		case b := <-fp.received:
			got = append(got, b...)
		case <-time.After(50 * time.Millisecond):
		}
	}
	return got
}

// ---------- construtor de agente de teste ----------

func writeTokenFile(t *testing.T, dir, token string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, agent.TokenFileName), []byte(strings.TrimSpace(token)), 0o644); err != nil {
		t.Fatalf("token.txt: %v", err)
	}
}

// startRails sobe o fake Rails num httptest Server de verdade.
func startRails(t *testing.T, rails *fakeRails) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(rails)
	t.Cleanup(srv.Close)
	return srv
}

func newTestAgent(t *testing.T, dir, baseURL string, rails *fakeRails) *agent.Agent {
	t.Helper()
	rc, err := agent.LoadRuntime(filepath.Join(dir, "printer-agent"))
	if err != nil {
		t.Fatalf("agent.LoadRuntime: %v", err)
	}
	client := agent.NewClient(baseURL, func() string { return agent.ReadToken(rc.TokenPath) })
	log := agent.NewNullLogger()
	dbg := agent.NewDebugBuffer()
	upd := agent.NewUpdater(rc.Dir, rc.ExePath, nil, log, dbg)
	upd.BindClient(client)
	a := agent.NewAgent(rc, client, log, dbg, upd)
	return a
}

// waitFor espera condição com prazo — para o download async do updater.
func waitFor(t *testing.T, d time.Duration, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("prazo estourado esperando: %s", what)
}
