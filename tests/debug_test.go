package tests

import (
	"localissimo-print-agent/internal/agent"
	"strings"
	"testing"
)

// Teto de 200 entradas: estourou → descarta as mais antigas (anel — epic §6).
func TestDebugBufferRing(t *testing.T) {
	d := agent.NewDebugBuffer()
	for i := 0; i < 250; i++ {
		d.Addf("entrada %03d", i)
	}
	snap := d.Snapshot()
	if len(snap) != 200 {
		t.Fatalf("teto de 200: %d", len(snap))
	}
	// As 50 mais antigas (000–049) foram descartadas; a primeira é a 050.
	if !strings.Contains(snap[0], "entrada 050") {
		t.Fatalf("anel deveria começar na entrada 050: %s", snap[0])
	}
	if !strings.Contains(snap[199], "entrada 249") {
		t.Fatalf("última deveria ser a 249: %s", snap[199])
	}
}

// Estouro de 2000 chars trunca a MENSAGEM preservando o timestamp e mantendo
// a CAUDA (em erro de rede a causa costuma estar no fim — epic §6).
func TestDebugBufferTruncaPreservandoCauda(t *testing.T) {
	d := agent.NewDebugBuffer()
	causa := "dial tcp 192.168.0.50:9100: connect: connection refused (última linha com a causa real)"
	msg := strings.Repeat("prefixo longo ", 300) + causa
	d.Add(msg)
	snap := d.Snapshot()
	if len(snap) != 1 {
		t.Fatalf("uma entrada: %d", len(snap))
	}
	e := snap[0]
	if len(e) != agent.DebugMaxChars {
		t.Fatalf("entrada deve ficar em %d chars: %d", agent.DebugMaxChars, len(e))
	}
	// Timestamp ISO8601 UTC preservado no início (20 chars + espaço).
	if e[10] != 'T' || e[19] != 'Z' {
		t.Fatalf("timestamp ISO8601 no início: %q", e[:24])
	}
	if !strings.HasSuffix(e, causa) {
		t.Fatalf("cauda preservada: %q", e[len(e)-80:])
	}
	if !strings.Contains(e, "…") {
		t.Fatalf("marca de truncamento ausente")
	}
}

// Entradas normais ficam abaixo do teto e carregam o timestamp.
func TestDebugBufferFormato(t *testing.T) {
	d := agent.NewDebugBuffer()
	d.Addf("job %d (%s) → %s · %d bytes ok", 7, "kitchen", "network · 192.168.0.50:9100", 1423)
	e := d.Snapshot()[0]
	if len(e) >= agent.DebugMaxChars {
		t.Fatalf("entrada curta não trunca")
	}
	if !strings.Contains(e, "job 7 (kitchen)") || !strings.Contains(e, "1423 bytes ok") {
		t.Fatalf("conteúdo: %q", e)
	}
	// Formato ISO8601 UTC: 2026-11-16T12:34:56Z msg
	if len(e) < 21 || e[20] != ' ' || e[19] != 'Z' {
		t.Fatalf("timestamp UTC RFC3339: %q", e[:min(len(e), 30)])
	}
}

func TestDebugBufferSnapshotCopia(t *testing.T) {
	d := agent.NewDebugBuffer()
	d.Add("x")
	snap := d.Snapshot()
	snap[0] = "adulterado"
	if d.Snapshot()[0] == "adulterado" {
		t.Fatalf("Snapshot deve devolver cópia")
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
