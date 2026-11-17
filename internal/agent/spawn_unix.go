//go:build !windows

package agent

import "os/exec"

// No Unix o filho sobrevive à morte do pai (nada de pdeathsig) — o processo
// velho encerra logo após o Start e o novo fica órfão do init/supervisor.
func setSpawnAttrs(cmd *exec.Cmd) {}
