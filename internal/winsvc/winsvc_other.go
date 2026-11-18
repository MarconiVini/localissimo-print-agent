//go:build !windows

// Stub do harness de serviço para Linux/macOS: lá não existe SCM — o
// supervisor é systemd (install.sh, Restart=on-failure) ou launchd, que
// não exigem conversa nenhuma do processo (SIGTERM + exit code bastam, e
// o modo console já os trata). A única dependência externa do repo
// (golang.org/x/sys) fica confinada ao arquivo windows.
package winsvc

import "context"

// ServiceName — mesmo nome que o unit systemd usa (install.sh).
const ServiceName = "printer-agent"

// UnderSCM é sempre false fora do Windows: main segue pelo caminho de
// console com sinais.
func UnderSCM() bool { return false }

// Run não tem papel fora do Windows; existe só para o build compilar.
func Run(_ string, _ func(context.Context) error) int {
	return 0
}
