package agent

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"time"
)

// Writer network (epic §5): TCP RAW JetDirect — dial ~5s, write ~10s, close.
// Sucesso = payload COMPLETO escrito (loop de write); n < len ou erro =
// falha, sem ACK — comanda pela metade não existe.
const (
	NetDialTimeout   = 5 * time.Second
	netWriteTimeout  = 10 * time.Second
	DefaultPrintPort = 9100
)

type NetworkWriter struct{}

// PrinterAddr resolve host:porta da config — port null/≤0 → 9100 (epic §5).
func PrinterAddr(cfg DeviceConfig) string {
	port := cfg.Port
	if port <= 0 {
		port = DefaultPrintPort
	}
	return net.JoinHostPort(cfg.Host, strconv.Itoa(port))
}

func (NetworkWriter) Write(ctx context.Context, cfg DeviceConfig, payload []byte) error {
	if cfg.Host == "" {
		return errors.New("conexão network sem host na config — nada para onde escrever")
	}
	addr := PrinterAddr(cfg)
	d := net.Dialer{Timeout: NetDialTimeout}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return fmt.Errorf("dial %s: %w", addr, err)
	}
	defer conn.Close()

	deadline := time.Now().Add(netWriteTimeout)
	if cd, ok := ctx.Deadline(); ok && cd.Before(deadline) {
		deadline = cd
	}
	if err := conn.SetWriteDeadline(deadline); err != nil {
		return fmt.Errorf("deadline: %w", err)
	}
	total := 0
	for total < len(payload) {
		n, werr := conn.Write(payload[total:])
		total += n
		if werr != nil {
			return fmt.Errorf("write %s: %d/%d bytes: %w", addr, total, len(payload), werr)
		}
		if n == 0 {
			return fmt.Errorf("write %s: progresso nulo em %d/%d bytes", addr, total, len(payload))
		}
	}
	return nil
}
