package agent

import (
	"fmt"
	"sync"
	"time"
)

// Buffer de debug em memória (epic §6, decisão 18): uma variável — anel
// circular com o teto do contrato do servidor: 200 entradas × 2000 chars;
// estourou → descarta as mais antigas. Mantido SEMPRE (debug on ou off) —
// se o admin ligar o debug ao vivo, a rodada seguinte já sai com o
// histórico recente. Jamais contém payload/comanda, token ou dados de
// cliente — só ids, contadores, bytes e erros de infra (isso é contrato dos
// chamadores: Add/Addf nunca recebem conteúdo de negócio).
const (
	debugMaxEntries = 200
	DebugMaxChars   = 2000
	debugTsLayout   = "2006-01-02T15:04:05Z" // ISO8601 UTC, 20 chars
)

type DebugBuffer struct {
	mu      sync.Mutex
	entries []string
}

func NewDebugBuffer() *DebugBuffer {
	return &DebugBuffer{entries: make([]string, 0, debugMaxEntries)}
}

func (d *DebugBuffer) Addf(format string, args ...any) {
	d.Add(fmt.Sprintf(format, args...))
}

func (d *DebugBuffer) Add(msg string) {
	ts := time.Now().UTC().Format(debugTsLayout)
	entry := ts + " " + msg
	if len(entry) > DebugMaxChars {
		// Estouro trunca a MENSAGEM preservando o timestamp e mantendo a
		// CAUDA (epic §6: em erro de rede, a causa costuma estar no fim).
		// Trunca por RUNE (acentos do PT-BR não podem virar bytes órfãos).
		prefix := ts + " … "
		budget := DebugMaxChars - len(prefix)
		runes := []rune(msg)
		bytes := 0
		i := len(runes)
		for i > 0 {
			nb := len(string(runes[i-1]))
			if bytes+nb > budget {
				break
			}
			bytes += nb
			i--
		}
		entry = prefix + string(runes[i:])
		if len(entry) > DebugMaxChars {
			entry = entry[:DebugMaxChars]
		}
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.entries = append(d.entries, entry)
	if len(d.entries) > debugMaxEntries {
		d.entries = d.entries[len(d.entries)-debugMaxEntries:]
	}
}

// Snapshot devolve uma cópia (o POST viaja fora do lock).
func (d *DebugBuffer) Snapshot() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]string, len(d.entries))
	copy(out, d.entries)
	return out
}

func (d *DebugBuffer) Len() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.entries)
}
