//go:build windows

package agent

import (
	"os/exec"
	"syscall"
)

// CREATE_NEW_PROCESS_GROUP: a instância nova não recebe o Ctrl+C do velho
// (o grupo do console morre com o pai — epic §7.4: spawn confirmado é
// condição para encerrar, e o novo tem que ficar de pé).
func setSpawnAttrs(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: 0x200}
}
