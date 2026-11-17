package agent

import (
	"fmt"
	"os"
	"sync"
	"time"
)

// Log local rotacionado (epic §3): rodando como tarefa/serviço não há
// console — o agent.log é sempre ativo, independente do debug_mode, e
// acumula TUDO (linhas de evento + espelho do buffer de debug, via Sink
// do DebugBuffer) através das execuções. Rotação ~10 MB, mantém 1 anterior
// (agent.log.1): fecha → renomeia → reabre. Rename falhou (arquivo preso,
// típico no Windows) → segue escrevendo no mesmo arquivo e tenta de novo
// na próxima escrita/boot seguinte.
const (
	LogRotateBytes = 10 << 20 // ~10 MB (≈ 80 mil linhas de ~130 bytes)
	logSuffixPrev  = ".1"
)

type Logger struct {
	mu      sync.Mutex
	path    string
	f       *os.File
	written int64
	mirror  bool // espelha no stderr (visível quando há console)
	stub    bool // testes: não toca no disco
}

func OpenLogger(path string, mirror bool) (*Logger, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, fmt.Errorf("abrir log %s: %w", path, err)
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("stat do log %s: %w", path, err)
	}
	return &Logger{path: path, f: f, written: st.Size(), mirror: mirror}, nil
}

func NewNullLogger() *Logger { return &Logger{stub: true, mirror: false} }

func NewConsoleLogger() *Logger { return &Logger{stub: true, mirror: true} }

func (l *Logger) Printf(format string, args ...any) {
	l.write(time.Now().Format(time.RFC3339) + " " + fmt.Sprintf(format, args...) + "\n")
}

func (l *Logger) write(line string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.stub {
		if l.mirror {
			os.Stderr.WriteString(line)
		}
		return
	}
	// Passou do teto: roda (ou retenta — rename falho antes fica devendo).
	if l.written > LogRotateBytes {
		l.rotateLocked()
	}
	if l.f == nil {
		if nf, err := os.OpenFile(l.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644); err == nil {
			l.f = nf
			if st, err := nf.Stat(); err == nil {
				l.written = st.Size()
			}
		} else {
			return // sem log aberto e sem como abrir: engole (não pode derrubar o agente)
		}
	}
	n, _ := l.f.WriteString(line)
	l.written += int64(n)
	if l.mirror {
		os.Stderr.WriteString(line)
	}
}

// rotateLocked fecha → renomeia → reabre. Falha do rename (arquivo mapeado no
// Windows) deixa tudo como está: a escrita continua no mesmo arquivo e a
// rotação é tentada de novo na próxima escrita.
func (l *Logger) rotateLocked() {
	if l.f == nil || l.written <= LogRotateBytes {
		return
	}
	prev := l.path + logSuffixPrev
	l.f.Close()
	l.f = nil
	if err := os.Rename(l.path, prev); err != nil {
		// Reabre o mesmo arquivo — nada se perde, só a rotação que fica devendo.
		if nf, err := os.OpenFile(l.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644); err == nil {
			l.f = nf
		}
		return
	}
	nf, err := os.OpenFile(l.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return
	}
	l.f = nf
	l.written = 0
	// Marcador no arquivo novo: sem ele, quem abre o agent.log não sabe
	// quando/por que o histórico anterior virou agent.log.1.
	marker := time.Now().Format(time.RFC3339) + " --- log rotacionado (passou de ~10 MB; histórico anterior em agent.log.1) ---\n"
	if n, err := nf.WriteString(marker); err == nil {
		l.written += int64(n)
	}
}

func (l *Logger) Close() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f != nil {
		l.f.Close()
		l.f = nil
	}
}
