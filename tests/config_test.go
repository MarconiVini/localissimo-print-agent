package tests

import (
	"localissimo-print-agent/internal/agent"
	"os"
	"path/filepath"
	"testing"
)

// Precedência do servidor (epic §3): flag -server > config.json > default
// compilado. config.json quebrado não derruba o agente (avisa e segue).
func TestServerURLPrecedencia(t *testing.T) {
	dir := t.TempDir()
	rc, err := agent.LoadRuntime(filepath.Join(dir, "printer-agent"))
	if err != nil {
		t.Fatal(err)
	}
	warns := []string{}
	warn := func(f string, a ...any) { warns = append(warns, f) }

	// Sem flag, sem config.json → default compilado.
	if got := rc.ServerURL("", warn); got != agent.DefaultServer {
		t.Fatalf("default: %s", got)
	}

	// config.json → vence o default; barra final é trimada.
	os.WriteFile(rc.ServerConf, []byte(`{"server": "http://localhost:3000/"}`), 0o644)
	if got := rc.ServerURL("", warn); got != "http://localhost:3000" {
		t.Fatalf("config.json: %s", got)
	}

	// Flag -server → vence tudo (dev/tester aponta pro localhost).
	if got := rc.ServerURL("http://127.0.0.1:3000", warn); got != "http://127.0.0.1:3000" {
		t.Fatalf("flag: %s", got)
	}

	// JSON quebrado → avisa e cai no default (config opcional não bricka).
	os.WriteFile(rc.ServerConf, []byte(`{"server": `), 0o644)
	if got := rc.ServerURL("", warn); got != agent.DefaultServer {
		t.Fatalf("config quebrada: %s", got)
	}
	if len(warns) == 0 {
		t.Fatalf("config quebrada deveria avisar")
	}

	// server sem esquema → ignorado, cai no default.
	os.WriteFile(rc.ServerConf, []byte(`{"server": "localhost:3000"}`), 0o644)
	if got := rc.ServerURL("", nil); got != agent.DefaultServer {
		t.Fatalf("server sem esquema deveria ser ignorado: %s", got)
	}
}

// agent.NormalizeServer: exige http(s):// + host (epic §8: nunca inventa esquema;
// produção https, dev http).
func TestNormalizeServer(t *testing.T) {
	cases := map[string]string{
		"https://localissimo.com.br": "https://localissimo.com.br",
		"  https://x.com.br/  ":      "https://x.com.br",
		"http://localhost:3000///":   "http://localhost:3000",
		"":                           "",
		"localhost:3000":             "",
		"ftp://x.com":                "",
		"https://":                   "",
		"://sem-esquema":             "",
	}
	for in, want := range cases {
		if got := agent.NormalizeServer(in); got != want {
			t.Errorf("agent.NormalizeServer(%q) = %q; quero %q", in, got, want)
		}
	}
}

func TestLoadRuntimePaths(t *testing.T) {
	rc, err := agent.LoadRuntime("/opt/localissimo/printer-agent")
	if err != nil {
		t.Fatal(err)
	}
	if rc.Dir != "/opt/localissimo" {
		t.Fatalf("dir: %s", rc.Dir)
	}
	if rc.TokenPath != "/opt/localissimo/token.txt" ||
		rc.ServerConf != "/opt/localissimo/config.json" ||
		rc.LogPath != "/opt/localissimo/agent.log" ||
		rc.LockPath != "/opt/localissimo/agent.lock" {
		t.Fatalf("caminhos ao lado do binário (portável nos 3 OS): %#v", rc)
	}
	if _, err := agent.LoadRuntime(""); err == nil {
		t.Fatalf("exePath vazio → erro")
	}
}

func TestReadToken(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, agent.TokenFileName)
	if got := agent.ReadToken(path); got != "" {
		t.Fatalf("arquivo ausente → token vazio: %q", got)
	}
	os.WriteFile(path, []byte("  token-secret-123\n\n"), 0o644)
	if got := agent.ReadToken(path); got != "token-secret-123" {
		t.Fatalf("trim: %q", got)
	}
	os.WriteFile(path, []byte("   \n"), 0o644)
	if got := agent.ReadToken(path); got != "" {
		t.Fatalf("arquivo só de espaço → vazio: %q", got)
	}
}

// Build sujo ignora auto-update (epic §7.4.1).
func TestVersionFormats(t *testing.T) {
	for _, ok := range []string{"0.1.0", "v0.1.0", "1.2.3"} {
		if !agent.IsValidReleaseVersion(ok) {
			t.Errorf("%q deveria casar", ok)
		}
	}
	for _, bad := range []string{"0.0.0-dev", "0.1.0-rc1", "", "v", "1.2", "01.02.03-"} {
		if agent.IsValidReleaseVersion(bad) {
			t.Errorf("%q não deveria casar", bad)
		}
	}
}
