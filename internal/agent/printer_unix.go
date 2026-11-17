//go:build !windows

package agent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// Linux/macOS (epic §2): descoberta via CUPS (lpstat), writer usb via
// `lp -d <fila> -o raw` com o payload no stdin — exit 0 = sucesso
// (tudo-ou-nada), exit ≠ 0 = falha com stderr no log/entrada de debug.
// Servidor headless sem CUPS → só writer network (lpstat ausente devolve
// report vazio, nunca erro fatal).

// discoverPrinters coleta as filas do CUPS e devolve as evidências do
// caminho em notes — sem lpstat, lpstat falhando ou fila vazia, o log do
// boot diz qual dos três foi (nada de "0 coletada(s)" sem motivo).
func discoverPrinters(ctx context.Context) ([]map[string]string, []string) {
	if _, err := exec.LookPath("lpstat"); err != nil {
		// Sem CUPS: report vazio (o boot posta stored 0 e segue — o device
		// aparece no painel, só sem sugestões de impressora).
		return nil, []string{"lpstat não encontrado (CUPS ausente?) — report de impressoras vazio; conexão network continua disponível"}
	}
	pout, err := exec.CommandContext(ctx, "lpstat", "-p").Output()
	if err != nil {
		return nil, []string{fmt.Sprintf("lpstat -p falhou: %v%s — report de impressoras vazio", err, lpstatStderr(err))}
	}
	vout, verr := exec.CommandContext(ctx, "lpstat", "-v").Output()
	var notes []string
	if verr != nil {
		// -v falho não derruba a coleta (nomes saem do -p); só perde Host/Porta.
		notes = append(notes, fmt.Sprintf("lpstat -v falhou: %v — Host/Porta das filas ficam de fora", verr))
	}
	var vtext string
	if verr == nil {
		vtext = string(vout)
	}
	names := ParseLpstatP(string(pout))
	uris := ParseLpstatV(vtext)
	notes = append(notes, fmt.Sprintf("CUPS: %d fila(s) visível(is)", len(names)))
	return CupsEntries(names, uris), notes
}

// lpstatStderr extrai o stderr de uma falha do lpstat — o exit code sozinho
// não diz nada; "Pausado" ou "Filtro falhou" sim. Truncado: stderr de filtro
// CUPS pode vir kilométrico.
func lpstatStderr(err error) string {
	var ee *exec.ExitError
	if errors.As(err, &ee) && len(ee.Stderr) > 0 {
		return " (stderr: " + clampLine(strings.TrimSpace(string(ee.Stderr)), 200) + ")"
	}
	return ""
}

// writeUSBPlatform despacha o writer usb do OS (matriz epic §2).
func writeUSBPlatform(ctx context.Context, printerName string, payload []byte) error {
	return writeUSBUnix(ctx, printerName, payload)
}

// writeUSBUnix imprime na fila RAW do CUPS pelo nome exato da config.
// Com teto de tempo (o mesmo do spooler no Windows): lp pendurado (CUPS
// travado, device USB fantasma) não pode congelar o loop do agente —
// CommandContext mata o processo no timeout/encerramento.
func writeUSBUnix(ctx context.Context, printerName string, payload []byte) error {
	if strings.TrimSpace(printerName) == "" {
		return errors.New("conexão usb sem printer_name na config")
	}
	lp, err := exec.LookPath("lp")
	if err != nil {
		return errors.New("lp não encontrado (fila CUPS RAW exigida para usb) — use conexão network ou crie a fila: lpadmin -p <fila> -E -v <device> -m raw")
	}
	wctx, cancel := context.WithTimeout(ctx, usbWriteTimeout)
	defer cancel()
	cmd := exec.CommandContext(wctx, lp, "-d", printerName, "-o", "raw")
	cmd.Stdin = bytes.NewReader(payload)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("write usb cancelado (encerramento): %w", ctx.Err())
		}
		if wctx.Err() != nil {
			return fmt.Errorf("lp -d %s: sem resposta em %s (morto pelo timeout) — job segue pending (reimprime)", printerName, usbWriteTimeout)
		}
		tail := stderr.String()
		if len(tail) > 300 {
			tail = tail[len(tail)-300:]
		}
		return fmt.Errorf("lp -d %s: %w (stderr: %s)", printerName, err, strings.TrimSpace(tail))
	}
	return nil
}
