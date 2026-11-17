//go:build windows

package agent

import (
	"fmt"
	"path/filepath"
	"syscall"
	"time"
	"unsafe"
)

// Instância única no Windows (epic §8): mutex nomeado. O nome deriva da
// pasta do binário (duas pastas = dois devices legítimos; mesma pasta duas
// vezes = bloqueia). Liberado pelo kernel quando o processo morre.
type InstanceLock struct {
	handle syscall.Handle
}

var kernel32 = syscall.NewLazyDLL("kernel32.dll")

var (
	procCreateMutexW = kernel32.NewProc("CreateMutexW")
	procReleaseMutex = kernel32.NewProc("ReleaseMutex")
	procCloseHandle  = kernel32.NewProc("CloseHandle")
)

const (
	errAlreadyExists = syscall.Errno(183)
	mutexNamePrefix  = `Global\localissimo-print-agent-`
)

func mutexNameFor(dir string) string {
	// O caminho pode ter caracteres proibidos no nome de objeto; um hash
	// curto resolve e mantém a unicidade por pasta.
	const fnvBasis = 1469598103934665603
	var h uint64 = fnvBasis
	for i := 0; i < len(dir); i++ {
		h ^= uint64(dir[i])
		h *= 1099511628211
	}
	return fmt.Sprintf("%s%x", mutexNamePrefix, h)
}

func AcquireInstanceLock(path string) (*InstanceLock, bool, error) {
	dir := filepath.Dir(path)
	name16, err := syscall.UTF16PtrFromString(mutexNameFor(dir))
	if err != nil {
		return nil, false, fmt.Errorf("nome do mutex: %w", err)
	}
	h, _, callErr := procCreateMutexW.Call(0, 0, uintptr(unsafe.Pointer(name16)))
	const invalidHandleValue = ^uintptr(0)
	if h == 0 || h == invalidHandleValue {
		return nil, false, fmt.Errorf("CreateMutex: %v", callErr)
	}
	if callErr == errAlreadyExists {
		procCloseHandle.Call(h)
		return nil, false, nil // já há instância viva
	}
	return &InstanceLock{handle: syscall.Handle(h)}, true, nil
}

func (l *InstanceLock) Release() {
	if l == nil {
		return
	}
	procReleaseMutex.Call(uintptr(l.handle))
	procCloseHandle.Call(uintptr(l.handle))
	l.handle = 0
}

func RetryLock(path string, maxWait time.Duration, sleep func(time.Duration)) (*InstanceLock, error) {
	deadline := time.Now().Add(maxWait)
	for {
		l, ok, err := AcquireInstanceLock(path)
		if err != nil || ok {
			return l, err
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("mutex não liberado em %s — outro agente continua rodando", maxWait)
		}
		sleep(500 * time.Millisecond)
	}
}

// SimulateCrash fecha o handle sem liberar o mutex — reproduz kill -9
// (o kernel solta; lock órfão é inofensivo). Gancho dos testes.
func (l *InstanceLock) SimulateCrash() {
	if l == nil {
		return
	}
	procCloseHandle.Call(uintptr(l.handle))
	l.handle = 0
}
