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

func discoverPrinters(ctx context.Context) []map[string]string {
	if _, err := exec.LookPath("lpstat"); err != nil {
		// Sem CUPS: report vazio (o boot posta stored 0 e segue — o device
		// aparece no painel, só sem sugestões de impressora).
		return nil
	}
	pout, err := exec.CommandContext(ctx, "lpstat", "-p").Output()
	if err != nil {
		return nil
	}
	vout, verr := exec.CommandContext(ctx, "lpstat", "-v").Output()
	var vtext string
	if verr == nil {
		vtext = string(vout)
	}
	names := ParseLpstatP(string(pout))
	uris := ParseLpstatV(vtext)
	return CupsEntries(names, uris)
}

// writeUSBPlatform despacha o writer usb do OS (matriz epic §2).
func writeUSBPlatform(ctx context.Context, printerName string, payload []byte) error {
	return writeUSBUnix(ctx, printerName, payload)
}

// writeUSBUnix imprime na fila RAW do CUPS pelo nome exato da config.
func writeUSBUnix(ctx context.Context, printerName string, payload []byte) error {
	if strings.TrimSpace(printerName) == "" {
		return errors.New("conexão usb sem printer_name na config")
	}
	lp, err := exec.LookPath("lp")
	if err != nil {
		return errors.New("lp não encontrado (fila CUPS RAW exigida para usb) — use conexão network ou crie a fila: lpadmin -p <fila> -E -v <device> -m raw")
	}
	cmd := exec.CommandContext(ctx, lp, "-d", printerName, "-o", "raw")
	cmd.Stdin = bytes.NewReader(payload)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		tail := stderr.String()
		if len(tail) > 300 {
			tail = tail[len(tail)-300:]
		}
		return fmt.Errorf("lp -d %s: %w (stderr: %s)", printerName, err, strings.TrimSpace(tail))
	}
	return nil
}
