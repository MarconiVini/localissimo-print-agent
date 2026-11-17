package tests

import (
	"localissimo-print-agent/internal/agent"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Rotação do agent.log (epic §3): ~10 MB, mantém 1 anterior — fecha →
// renomeia → reabre.
func TestLoggerRotacao(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent.log")
	l, err := agent.OpenLogger(path, false)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	// Estoura 10 MB de verdade (rápido e sem mocks).
	big := strings.Repeat("x", 8192) + "\n"
	for i := 0; i < 1500; i++ { // ~12.3 MB
		l.Printf("%05d %s", i, big)
	}
	if _, err := os.Stat(path + ".1"); err != nil {
		t.Fatalf("agent.log.1 deveria existir após passar de 10 MB: %v", err)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Size() > agent.LogRotateBytes {
		t.Fatalf("agent.log novo deveria ter começado pequeno: %d bytes", st.Size())
	}

	// A rotação preserva o histórico no .1.
	data, err := os.ReadFile(path + ".1")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "00000") {
		t.Fatalf("histórico no .1: %s...", string(data[:60]))
	}

	// Escreve mais um pouco e confere que o arquivo novo segue crescendo.
	l.Printf("linha pós-rotação")
	data2, _ := os.ReadFile(path)
	if !strings.Contains(string(data2), "linha pós-rotação") {
		t.Fatalf("escrita pós-rotação perdida")
	}
	// O arquivo novo abre com o marcador de rotação (quando/por que o
	// histórico virou agent.log.1).
	if !strings.Contains(string(data2), "log rotacionado") {
		t.Fatalf("marcador de rotação ausente no arquivo novo: %q", string(data2[:min(len(data2), 120)]))
	}
}

// Rename falhou (arquivo preso — típico Windows) → segue escrevendo no
// MESMO arquivo e a rotação é tentada de novo (epic §3).
func TestLoggerRotacaoFalhaTolerada(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent.log")
	l, err := agent.OpenLogger(path, false)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	// Obstáculo: agent.log.1 é um DIRETÓRIO — o rename falha.
	if err := os.Mkdir(path+".1", 0o755); err != nil {
		t.Fatal(err)
	}
	big := strings.Repeat("y", 8192) + "\n"
	for i := 0; i < 1500; i++ { // ~12.3 MB: passa do teto com a rotação falhando
		l.Printf("%05d %s", i, big)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("agent.log sumiu?! rename falho não pode perder o arquivo: %v", err)
	}

	// Removido o obstáculo, a próxima escrita roda a rotação pendente.
	if err := os.Remove(path + ".1"); err != nil {
		t.Fatal(err)
	}
	l.Printf("destrava")
	waitFor(t, 2*time.Second, func() bool {
		_, err := os.Stat(path + ".1")
		return err == nil
	}, "rotação retries até agent.log.1 existir")
}

func TestLoggerStubNaoTocaDisco(t *testing.T) {
	l := agent.NewNullLogger()
	l.Printf("sem disco") // não pode panicar nem criar arquivo
}
