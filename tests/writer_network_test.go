package tests

import (
	"context"
	"io"
	"localissimo-print-agent/internal/agent"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"
)

// agent.Writer network feliz: payload COMPLETO chega (loop de write — epic §5) e
// a conexão fecha ao final.
func TestNetworkWriterPayloadCompleto(t *testing.T) {
	fp := startFakePrinter(t)
	h, p := fp.hostPort()
	payload := append([]byte{0x1b, 0x40}, []byte(strings.Repeat("COMANDA\n", 400))...) // ~3.2KB
	cfg := agent.DeviceConfig{Connection: "network", Host: h, Port: p}

	if err := (agent.NetworkWriter{}).Write(context.Background(), cfg, payload); err != nil {
		t.Fatalf("write: %v", err)
	}
	got := fp.waitBytes(t, len(payload))
	if string(got) != string(payload) {
		t.Fatalf("payload diverge: %d vs %d bytes", len(got), len(payload))
	}
}

// Escrita incompleta = falha: o servidor lê um pedaço e fecha — n < len
// nunca pode parecer sucesso (comanda pela metade não existe, epic §5).
func TestNetworkWriterEscritaParcialFalha(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		// Lê 16 bytes e fecha na cara (impressora sem papel no meio do job).
		buf := make([]byte, 16)
		conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		_, _ = io.ReadFull(conn, buf)
		conn.Close()
	}()

	h, ps, _ := net.SplitHostPort(ln.Addr().String())
	pi, _ := strconv.Atoi(ps)
	cfg := agent.DeviceConfig{Connection: "network", Host: h, Port: pi}

	// Payload maior que o buffer do socket: com o leitor morto, o write
	// trava/quebra em algum ponto < len — e tem que virar ERRO.
	payload := []byte(strings.Repeat("x", 2<<20)) // 2 MB
	werr := agent.NetworkWriter{}.Write(context.Background(), cfg, payload)
	if werr == nil {
		t.Fatalf("servidor fechando cedo deve falhar o write")
	}
	if !strings.Contains(werr.Error(), "bytes") {
		t.Fatalf("erro deve citar bytes escritos: %v", werr)
	}
}

// Guardas: host vazio → erro claro; port null/≤0 → 9100 (epic §5).
func TestNetworkWriterDefaultsEGuardas(t *testing.T) {
	if err := (agent.NetworkWriter{}).Write(context.Background(), agent.DeviceConfig{Connection: "network", Host: ""}, []byte("x")); err == nil ||
		!strings.Contains(err.Error(), "host") {
		t.Fatalf("host vazio: %v", err)
	}
	if got := agent.PrinterAddr(agent.DeviceConfig{Host: "192.168.0.50", Port: 0}); got != "192.168.0.50:9100" {
		t.Fatalf("port null → 9100: %s", got)
	}
	if got := agent.PrinterAddr(agent.DeviceConfig{Host: "192.168.0.50", Port: -1}); got != "192.168.0.50:9100" {
		t.Fatalf("port ≤0 → 9100: %s", got)
	}
	if got := agent.PrinterAddr(agent.DeviceConfig{Host: "192.168.0.50", Port: 8180}); got != "192.168.0.50:8180" {
		t.Fatalf("port da config vence: %s", got)
	}
}

// Impressora morta: dial rápido e erro — sem travar a rodada.
func TestNetworkWriterConexaoRecusada(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	h, ps, _ := net.SplitHostPort(ln.Addr().String())
	pi, _ := strconv.Atoi(ps)
	ln.Close() // porta morta

	start := time.Now()
	err = agent.NetworkWriter{}.Write(context.Background(), agent.DeviceConfig{Host: h, Port: pi}, []byte("x"))
	if err == nil {
		t.Fatalf("porta morta deveria dar erro")
	}
	if elapsed := time.Since(start); elapsed > agent.NetDialTimeout {
		t.Fatalf("dial deveria falhar rápido, levou %v", elapsed)
	}
}
