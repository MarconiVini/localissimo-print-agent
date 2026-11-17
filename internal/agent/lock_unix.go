//go:build !windows

package agent

import (
	"errors"
	"fmt"
	"os"
	"syscall"
	"time"
)

// Instância única (epic §8): flock exclusivo em agent.lock. Liberado pelo
// kernel em crash — lock órfão é inofensivo. Dois agentes = todo job sai em
// dobro, então não adquiriu → sair com erro claro (quem religa é o
// autostart/supervisor ou o retry pós-update).
type InstanceLock struct {
	f *os.File
}

func AcquireInstanceLock(path string) (*InstanceLock, bool, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, false, fmt.Errorf("abrir lock %s: %w", path, err)
	}
	err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if err == nil {
		return &InstanceLock{f: f}, true, nil
	}
	if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
		f.Close()
		return nil, false, nil // já há instância viva
	}
	f.Close()
	return nil, false, fmt.Errorf("flock %s: %w", path, err)
}

func (l *InstanceLock) Release() {
	if l == nil || l.f == nil {
		return
	}
	syscall.Flock(int(l.f.Fd()), syscall.LOCK_UN)
	l.f.Close()
	l.f = nil
}

// RetryLock espera o lock vir (usado pós-update: o processo velho ainda pode
// estar vivo por alguns instantes — epic §7.4).
func RetryLock(path string, maxWait time.Duration, sleep func(time.Duration)) (*InstanceLock, error) {
	deadline := time.Now().Add(maxWait)
	for {
		l, ok, err := AcquireInstanceLock(path)
		if err != nil || ok {
			return l, err
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("lock %s não liberado em %s — outro agente continua rodando", path, maxWait)
		}
		sleep(500 * time.Millisecond)
	}
}

// SimulateCrash fecha o descritor SEM liberar o flock — reproduz o estado
// de um kill -9 (o kernel solta o lock; lock órfão é inofensivo). Também é
// o gancho usado pelos testes.
func (l *InstanceLock) SimulateCrash() {
	if l == nil || l.f == nil {
		return
	}
	l.f.Close()
	l.f = nil
}
