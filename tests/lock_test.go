//go:build !windows

package tests

import (
	"localissimo-print-agent/internal/agent"
)

import (
	"path/filepath"
	"testing"
	"time"
)

// Instância única (epic §8): 2º acquire na mesma pasta NÃO passa — dois
// agentes = todo job sai em dobro. Release devolve o lock ao próximo.
func TestInstanceLockExclusivo(t *testing.T) {
	dir := t.TempDir()
	lockPath := filepath.Join(dir, agent.LockFileName)

	l1, ok, err := agent.AcquireInstanceLock(lockPath)
	if err != nil || !ok {
		t.Fatalf("1º acquire: ok=%v err=%v", ok, err)
	}

	// Segunda instância na mesma pasta é bloqueada.
	_, ok2, err2 := agent.AcquireInstanceLock(lockPath)
	if err2 != nil {
		t.Fatalf("2º acquire err: %v", err2)
	}
	if ok2 {
		t.Fatalf("2º acquire NÃO deveria passar (nunca duas instâncias)")
	}

	// Release libera para o próximo.
	l1.Release()
	l3, ok3, err3 := agent.AcquireInstanceLock(lockPath)
	if err3 != nil || !ok3 {
		t.Fatalf("acquire após release: ok=%v err=%v", ok3, err3)
	}
	l3.Release()
}

// Lock órfão (crash deixa o arquivo) é inofensivo: o kernel solta o flock.
func TestInstanceLockOrfaoInofensivo(t *testing.T) {
	dir := t.TempDir()
	lockPath := filepath.Join(dir, agent.LockFileName)
	// Cria o arquivo e "crasha" (sem release): novo acquire na mesma pasta
	// do MESMO processo... na verdade o flock é por descritor aberto.
	l1, ok, err := agent.AcquireInstanceLock(lockPath)
	if err != nil || !ok {
		t.Fatal("setup")
	}
	// Simula crash: fecha o fd sem funlock (o kernel solta).
	l1.SimulateCrash()
	if _, ok2, _ := agent.AcquireInstanceLock(lockPath); !ok2 {
		t.Fatalf("lock órfão travou a instância nova")
	}
}

// Retry pós-update (epic §7.4.5): espera o lock vir por até ~30s; não veio →
// erro claro (nunca segue sem lock).
func TestRetryLock(t *testing.T) {
	dir := t.TempDir()
	lockPath := filepath.Join(dir, agent.LockFileName)

	l1, ok, _ := agent.AcquireInstanceLock(lockPath)
	if !ok {
		t.Fatal("setup")
	}

	// Prazo curto no teste: falha clara, sem seguir sem lock.
	fast := func(d time.Duration) {}
	if _, err := agent.RetryLock(lockPath, 30*time.Millisecond, fast); err == nil {
		t.Fatalf("agent.RetryLock deveria falhar com o lock preso")
	}

	// O velho solta no meio do retry → o novo adquire.
	go func() {
		time.Sleep(10 * time.Millisecond)
		l1.Release()
	}()
	slept := 0
	pacing := func(d time.Duration) { slept++ }
	l2, err := agent.RetryLock(lockPath, 5*time.Second, pacing)
	if err != nil {
		t.Fatalf("agent.RetryLock após release: %v", err)
	}
	if slept == 0 {
		t.Fatalf("retry deveria ter esperado ao menos uma rodada")
	}
	l2.Release()
}
