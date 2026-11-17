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
//
// Sink (espelho local): toda entrada também é repassada ao log local do
// arquivo — o agent.log guarda o histórico completo de TODAS as execuções
// (rotação 10 MB), mesmo com debug_mode=false. O ENVIO ao servidor segue
// gateado pela config (Agent.sendDebug só posta com debug_mode=true — o
// buffer chega inteiro na 1ª rodada depois que o admin liga). O Sink recebe
// a mensagem SEM o timestamp UTC do anel (o logger acrescenta o dele,
// local). Chamado fora do lock — nunca chama de volta no buffer.
const (
	debugMaxEntries = 200
	DebugMaxChars   = 2000
	debugTsLayout   = "2006-01-02T15:04:05Z" // ISO8601 UTC, 20 chars
)

type DebugBuffer struct {
	mu      sync.Mutex
	entries []string
	Sink    func(msg string) // nil = sem espelho (testes)
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
	d.entries = append(d.entries, entry)
	if len(d.entries) > debugMaxEntries {
		d.entries = d.entries[len(d.entries)-debugMaxEntries:]
	}
	d.mu.Unlock()
	// Espelho pro log local vai SEM o timestamp do anel (o logger põe o
	// dele) e SEM truncamento (o teto de 2000 chars é contrato do POST,
	// não do arquivo). Fora do lock — o logger não chama de volta.
	if d.Sink != nil {
		d.Sink(msg)
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
