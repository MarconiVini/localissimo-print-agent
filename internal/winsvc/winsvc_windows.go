//go:build windows

// Package winsvc é o harness do printer-agent como serviço nativo do
// Windows: o SCM (Service Control Manager) é o supervisor — sobe no boot
// sem login, religa com backoff em falha (sc failure + failureflag) e
// cancela o ctx do agente no Stop/Shutdown. Não há watcher: quem vigia é
// o próprio OS. Nada daqui roda em Linux/macOS (stub em winsvc_other.go;
// lá o supervisor é systemd, que não exige conversa nenhuma do processo).
package winsvc

import (
	"context"
	"fmt"
	"os"
	"sync"

	"golang.org/x/sys/windows/svc"
)

// ServiceName é o nome fixo do serviço no SCM — o install.bat (packaging/)
// cria e referencia com o mesmo nome.
const ServiceName = "printer-agent"

// UnderSCM: este processo foi iniciado pelo gerenciador de serviços? Sob o
// SCM não existe teclado — a via de mão dupla é o canal do serviço; o modo
// console (com Ctrl+C) continua acessível com -debug.
func UnderSCM() bool {
	v, _ := svc.IsWindowsService()
	return v
}

// handler adapta o run do agente ao protocolo do SCM: StartPending →
// Running (aceita Stop/Shutdown) → o ctx é cancelado → StopPending →
// STOPPED. O exit code reportado é o que o SCM usa para decidir religar:
// 0 = parada limpa (não religa); ≠ 0 = falha (religa — com failureflag=1,
// mesmo em stop "limpo com erro", que é o caso do encerramento pós-update).
type handler struct {
	run func(ctx context.Context) error

	mu       sync.Mutex
	exitCode uint32
}

func (h *handler) setExit(code uint32) {
	h.mu.Lock()
	h.exitCode = code
	h.mu.Unlock()
}

// ExitCode devolve o código que o serviço entregou ao SCM (0 = limpo).
func (h *handler) ExitCode() uint32 {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.exitCode
}

func (h *handler) Execute(args []string, req <-chan svc.ChangeRequest, status chan<- svc.Status) (bool, uint32) {
	status <- svc.Status{State: svc.StartPending}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		// Pânico no run derruba o processo inteiro sem reportar STOPPED —
		// crash clássico, que o SCM religa pelas failure actions de sempre.
		done <- h.run(ctx)
	}()

	status <- svc.Status{State: svc.Running, Accepts: svc.AcceptStop | svc.AcceptShutdown}

	for {
		select {
		case err := <-done:
			// Run acabou sem stop pedido: por construção só o pós-update
			// faz isso (errUpdateExit) — sai com falha para o SCM religar
			// já no binário novo (~10s, actions do sc failure).
			if err != nil {
				h.setExit(1)
				return true, 1
			}
			return true, 0
		case c := <-req:
			switch c.Cmd {
			case svc.Interrogate:
				status <- c.CurrentStatus
			case svc.Stop, svc.Shutdown:
				status <- svc.Status{State: svc.StopPending}
				cancel()
				// Espera o loop encerrar (teto natural: usbWriteTimeout
				// de 30s; o SCM por cima usa o timeout de stop dele).
				if err := <-done; err != nil {
					h.setExit(1)
					return true, 1
				}
				return true, 0
			default:
				// sem Pause/Continue — o agente não tem estados extras
			}
		}
	}
}

// Run opera name sob o SCM e devolve o exit code do processo: 0 se a parada
// foi limpa, 1 em falha (inclui o encerramento pós-update — religar é o
// contrato do supervisor).
func Run(name string, run func(ctx context.Context) error) int {
	h := &handler{run: run}
	if err := svc.Run(name, h); err != nil {
		fmt.Fprintln(os.Stderr, "serviço:", err)
		return 1
	}
	return int(h.ExitCode())
}
