package tests

import (
	"fmt"
	"localissimo-print-agent/internal/agent"
	"os"
	"strings"
	"testing"
)

// Parsing do lpstat -p: nomes de fila, sem duplicata (epic §12 — fixtures).
func TestParseLpstatP(t *testing.T) {
	out := "printer POS80 is idle.  enabled since dom 16 nov 2026 10:00:00 -03 (BRT)\n" +
		"printer Tomate-MDK is idle.  enabled since seg 10 nov 2026 09:00:00 -03 (BRT)\n" +
		"printer POS80 now printing POS80-42.  enabled since dom 16 nov 2026 10:00:00 -03 (BRT)\n"
	got := agent.ParseLpstatP(out)
	if len(got) != 2 || got[0] != "POS80" || got[1] != "Tomate-MDK" {
		t.Fatalf("agent.ParseLpstatP: %v", got)
	}
	if len(agent.ParseLpstatP("")) != 0 {
		t.Fatalf("saída vazia → lista vazia")
	}
}

// Parsing do lpstat -v: socket:// com e sem porta, usb://, dev://, ipp://.
func TestParseLpstatV(t *testing.T) {
	out := "device for POS80: socket://192.168.0.50:9100\n" +
		"device for Tomate-MDK: socket://tomate.local\n" +
		"device for POSUSB: usb://POS-80/serial?location=x\n" +
		"device for RAWDEV: ///dev/usb/lp0\n" +
		"device for IPPONE: ipp://10.0.0.9:631/ipp/print\n"
	got := agent.ParseLpstatV(out)
	if got["POS80"] != "socket://192.168.0.50:9100" || got["Tomate-MDK"] != "socket://tomate.local" ||
		got["POSUSB"] != "usb://POS-80/serial?location=x" || got["RAWDEV"] != "///dev/usb/lp0" ||
		got["IPPONE"] != "ipp://10.0.0.9:631/ipp/print" {
		t.Fatalf("agent.ParseLpstatV: %#v", got)
	}
}

// agent.CupsEntries: Host/Porta do URI socket:// (Porta numérica como STRING —
// contrato cravado); porta desconhecida → omitir Porta (epic §4);
// PortaNome = fila do CUPS (é ela que o writer usb usa no lp -d).
func TestCupsEntries(t *testing.T) {
	names := []string{"POS80", "Tomate-MDK", "POSUSB", "RAWDEV", "ORFANA"}
	uris := map[string]string{
		"POS80":      "socket://192.168.0.50:9100",
		"Tomate-MDK": "socket://tomate.local",
		"POSUSB":     "usb://POS-80",
		"RAWDEV":     "///dev/usb/lp0",
	}
	entries := agent.CupsEntries(names, uris)
	byName := map[string]map[string]string{}
	for _, e := range entries {
		byName[e["name"]] = e
	}

	pos := byName["POS80"]
	if pos["Host"] != "192.168.0.50" || pos["Porta"] != "9100" {
		t.Fatalf("POS80: %#v (Porta tem que ser string)", pos)
	}
	if pos["PortaNome"] != "POS80" {
		t.Fatalf("PortaNome = fila do CUPS: %#v", pos)
	}
	tom := byName["Tomate-MDK"]
	if tom["Host"] != "tomate.local" {
		t.Fatalf("Tomate Host: %#v", tom)
	}
	if _, ok := tom["Porta"]; ok {
		t.Fatalf("porta desconhecida → omitir Porta (epic §4): %#v", tom)
	}
	usb := byName["POSUSB"]
	if _, ok := usb["Host"]; ok {
		t.Fatalf("usb:// não tem Host: %#v", usb)
	}
	raw := byName["RAWDEV"]
	if _, ok := raw["Host"]; ok {
		t.Fatalf("dev:// não tem Host: %#v", raw)
	}
	if len(byName["ORFANA"]) != 1 {
		t.Fatalf("fila sem URI entra só com name: %#v", byName["ORFANA"])
	}
}

// Windows: chaves cravadas do contrato (name/PortaNome/Host/Porta/driver),
// todos string; porta TCP resolvida pelo lookup; porta UNC não resolve.
func TestWindowsEntries(t *testing.T) {
	rows := []agent.WinPrinterRow{
		{Name: "POS-80", PortName: "IP_192.168.0.50", DriverName: "POS-80 Printer", Status: 128},
		{Name: "POS-80 USB", PortName: "USB001", DriverName: "POS-80", Status: 0},
		{Name: "Tomate na rede", PortName: `\\servidor\Tomate`, DriverName: "Tomate MDK", Status: 16},
	}
	ports := []agent.WinPortRow{
		{Name: "IP_192.168.0.50", Description: "Standard TCP/IP Port"},
		{Name: "USB001", Description: "USB001 Virtual printer port for USB"},
	}
	lookup := func(portName string) (string, string) {
		if portName == "IP_192.168.0.50" {
			return "192.168.0.50", "9100"
		}
		return "", ""
	}
	entries := agent.WindowsEntries(rows, ports, lookup)
	if len(entries) != 3 {
		t.Fatalf("3 impressoras: %#v", entries)
	}
	net1 := entries[0]
	if net1["name"] != "POS-80" || net1["PortaNome"] != "IP_192.168.0.50" ||
		net1["Host"] != "192.168.0.50" || net1["Porta"] != "9100" || net1["driver"] != "POS-80 Printer" {
		t.Fatalf("entrada TCP: %#v", net1)
	}
	if net1["Status"] != "offline" {
		t.Fatalf("status textual: %#v", net1)
	}
	usb1 := entries[1]
	if _, ok := usb1["Host"]; ok {
		t.Fatalf("USB001 não tem Host: %#v", usb1)
	}
	unc := entries[2]
	if _, ok := unc["Host"]; ok {
		t.Fatalf("porta UNC \\\\servidor não resolve Host: %#v", unc)
	}
	if unc["Status"] != "sem papel" {
		t.Fatalf("status 16 = sem papel: %#v", unc)
	}
}

func isIntLike(s string) bool {
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return len(s) > 0
}

// agent.TCPLookupFromPorts: só porta descrita como TCP/IP é candidata; registry
// resolve primeiro; fallback pelo nome IP_*.
func TestTcpLookupFromPorts(t *testing.T) {
	ports := []agent.WinPortRow{
		{Name: "IP_10.1.2.3", Description: "Standard TCP/IP Port"},
		{Name: "USB001", Description: "Virtual printer port"},
	}
	resolve := func(name string) (string, string) {
		if name == "IP_10.1.2.3" {
			return "10.1.2.3", "9100"
		}
		return "", ""
	}
	lookup := agent.TCPLookupFromPorts(ports, resolve)
	if h, p := lookup("IP_10.1.2.3"); h != "10.1.2.3" || p != "9100" {
		t.Fatalf("lookup registry: %s %s", h, p)
	}
	if h, _ := lookup("USB001"); h != "" {
		t.Fatalf("USB não é TCP: %s", h)
	}

	// Sem registry (resolve nil): fallback parse do nome IP_<host>.
	lookup2 := agent.TCPLookupFromPorts(ports, nil)
	if h, p := lookup2("IP_192.168.7.7"); h != "192.168.7.7" || p != "9100" {
		t.Fatalf("fallback IP_*: %s %s", h, p)
	}

	// Porta sem cara de TCP não resolve nada.
	ports2 := []agent.WinPortRow{{Name: "LPT1:", Description: "Printer Port"}}
	lookup3 := agent.TCPLookupFromPorts(ports2, resolve)
	if h, _ := lookup3("LPT1:"); h != "" {
		t.Fatalf("LPT não é TCP: %s", h)
	}
}

// Filtros do report (epic §4): impressoras de arquivo/PDF/XPS fora, térmica
// primeiro (TCP RAW > USB > resto) e empate alfabético.
func TestFilterAndRank(t *testing.T) {
	entries := []map[string]string{
		{"name": "Microsoft Print to PDF"},
		{"name": "Microsoft XPS Document agent.Writer"},
		{"name": "PORTPROMPT:", "PortaNome": "PORTPROMPT:"},
		{"name": "FILE:", "PortaNome": "FILE:"},
		{"name": "Zebra Word", "driver": ""},
		{"name": "Word Generic", "driver": "Generic / Text Only"},
		{"name": "Tomate MDK-080", "Host": "192.168.0.51", "Porta": "9100", "driver": "Tomate MDK-080"},
		{"name": "POS-80 USB", "PortaNome": "USB001", "driver": "POS-80"},
		{"name": "PDF Exporter Pro", "driver": "PDF Exporter"},
	}
	got := agent.FilterAndRank(entries)
	names := []string{}
	for _, e := range got {
		names = append(names, e["name"])
	}
	want := []string{"Tomate MDK-080", "POS-80 USB", "Word Generic", "Zebra Word"}
	if strings.Join(names, "|") != strings.Join(want, "|") {
		t.Fatalf("ordem térmica errada: %v (TCP RAW primeiro, depois USB, depois resto alfabético)", names)
	}
	for _, n := range names {
		low := strings.ToLower(n)
		if strings.Contains(low, "pdf") || strings.Contains(low, "xps") ||
			strings.Contains(n, "PORTPROMPT") || strings.HasPrefix(n, "FILE:") {
			t.Fatalf("virtual vazou no report: %s", n)
		}
	}
}

// Teto de 50 aplicado DEPOIS da ordenação térmica (epic §4) — a térmica não
// sai do corte num balcão cheio de impressoras genéricas.
func TestFilterAndRankCap50(t *testing.T) {
	entries := []map[string]string{
		{"name": "NaoTermica-99", "driver": "Coletora qualquer"},
		{"name": "TermicaTCP", "Host": "10.0.0.9", "Porta": "9100"},
	}
	for i := 0; i < 60; i++ {
		entries = append(entries, map[string]string{"name": fmt.Sprintf("Gen-%02d", i)})
	}
	got := agent.FilterAndRank(entries)
	if len(got) != 50 {
		t.Fatalf("teto de 50: %d", len(got))
	}
	found := false
	for _, e := range got {
		if e["name"] == "TermicaTCP" {
			found = true
		}
	}
	if !found {
		t.Fatalf("térmica cortada pelo teto — ordenação deveria protegê-la")
	}
	// Sem name → descartada (o servidor descartaria também); nil → idem.
	if got := agent.FilterAndRank([]map[string]string{{"driver": "x"}, nil}); len(got) != 0 {
		t.Fatalf("entrada sem name/nil: %#v", got)
	}
}

func TestPrinterStatusText(t *testing.T) {
	if got := agent.PrinterStatusText(0); got != "0x0" {
		t.Fatalf("status 0: %s", got)
	}
	if got := agent.PrinterStatusText(128 | 16); !strings.Contains(got, "offline") || !strings.Contains(got, "sem papel") {
		t.Fatalf("bits combinados: %s", got)
	}
	if got := agent.PrinterStatusText(4096); got != "indisponível" {
		t.Fatalf("bit único: %s", got)
	}
	// Bits altos conforme o winspool.h do SDK — o mapeamento antigo dizia
	// "tampa aberta"/"intervenção"/"offline" nos bits errados (verificado
	// no header; bit errado vira diagnóstico errado no painel do balcão).
	bitsAltos := []struct {
		status uint32
		quer   string
	}{
		{0x00100000, "intervenção do usuário"},      // USER_INTERVENTION
		{0x00200000, "sem memória"},                 // OUT_OF_MEMORY
		{0x00400000, "tampa aberta"},                // DOOR_OPEN (antes não mapeado)
		{0x00800000, "servidor de impressão sumiu"}, // SERVER_UNKNOWN
	}
	for _, c := range bitsAltos {
		if got := agent.PrinterStatusText(c.status); got != c.quer {
			t.Errorf("status 0x%08x: quero %q, veio %q", c.status, c.quer, got)
		}
	}
	// IO_ACTIVE fica de fora de propósito (atividade normal) — bit só ele
	// cai no bruto 0x...
	if got := agent.PrinterStatusText(0x00000100); got != "0x100" {
		t.Errorf("bit fora da tabela deveria vir bruto: %q", got)
	}
}

func TestBuildReportBody(t *testing.T) {
	body := agent.BuildReportBody([]map[string]string{{"name": "POS-80", "Porta": "9100"}})
	if body["agent_version"] != agent.Version {
		t.Fatalf("agent_version: %#v", body["agent_version"])
	}
	switch body["os"].(string) {
	case "linux", "windows", "darwin":
	default:
		t.Fatalf("os: %#v", body["os"])
	}
	switch body["arch"].(string) {
	case "amd64", "arm64":
	default:
		t.Fatalf("arch: %#v", body["arch"])
	}
	printers, ok := body["printers"].([]map[string]string)
	if !ok || len(printers) != 1 || printers[0]["Porta"] != "9100" {
		t.Fatalf("printers no corpo: %#v", body["printers"])
	}
	hostname, _ := os.Hostname()
	if body["hostname"] != hostname {
		t.Fatalf("hostname: %#v vs %q", body["hostname"], hostname)
	}
}

// Resumo da coleta (epic §4 com evidências): o log do boot tem que explicar
// a diferença entre coletado do OS e enviado no report — coletou 4, filtro
// tirou 3 (2 virtuais + 1 sem nome), enviou 1, e o nome de cada descartada
// aparece (suporte sabe POR QUE a térmica não está na lista do painel).
func TestBuildDiscoveryResumeDescreveFiltro(t *testing.T) {
	raw := []map[string]string{
		{"name": "POS-80", "PortaNome": "USB001", "driver": "POS-80 Driver"},
		{"name": "Microsoft Print to PDF", "PortaNome": "PORTPROMPT:", "driver": "Microsoft Print To PDF"},
		{"name": "OneNote for Windows 10"},
		{"driver": "Fantasma"}, // sem name
	}
	res := agent.BuildDiscoveryResume(raw, []string{"spooler: 4 impressora(s)"})
	if res.RawCount != 4 {
		t.Fatalf("RawCount: %d", res.RawCount)
	}
	if len(res.Kept) != 1 || res.Kept[0]["name"] != "POS-80" {
		t.Fatalf("Kept: %#v", res.Kept)
	}
	if len(res.Dropped) != 3 {
		t.Fatalf("esperava 3 descartadas (2 virtuais + 1 sem nome), veio %v", res.Dropped)
	}
	joined := strings.Join(res.Notes, "\n")
	if !strings.Contains(joined, "spooler: 4 impressora(s)") {
		t.Fatalf("nota do OS perdida: %s", joined)
	}
	if !strings.Contains(joined, "filtro do report descartou 3") ||
		!strings.Contains(joined, "Microsoft Print to PDF") ||
		!strings.Contains(joined, "OneNote for Windows 10") ||
		!strings.Contains(joined, "(sem nome)") {
		t.Fatalf("nota de filtro incompleta: %s", joined)
	}
	if strings.Contains(joined, "nenhuma impressora encontrada") {
		t.Fatalf("raw>0 não pode ganhar nota de vazio: %s", joined)
	}
}

// Coleta vazia NUNCA é silenciosa: a nota do OS (spooler morto, sem lpstat,
// EnumPrinters falhou…) entra e a dica de ação completa o quadro.
func TestBuildDiscoveryResumeVazioExplica(t *testing.T) {
	res := agent.BuildDiscoveryResume(nil, []string{"EnumPrinters falhou: spooler parado"})
	if res.RawCount != 0 || len(res.Kept) != 0 || len(res.Dropped) != 0 {
		t.Fatalf("resumo de coleta vazia: %#v", res)
	}
	joined := strings.Join(res.Notes, "\n")
	if !strings.Contains(joined, "EnumPrinters falhou") {
		t.Fatalf("nota do OS perdida: %s", joined)
	}
	if !strings.Contains(joined, "nenhuma impressora encontrada") ||
		!strings.Contains(joined, "reinicie o agente") {
		t.Fatalf("falta dica de ação no vazio: %s", joined)
	}
}

// Filtro com memória: descartadas listadas, nenhuma descartada → nenhuma
// nota extra de filtro.
func TestFilterAndRankDetails(t *testing.T) {
	raw := []map[string]string{
		{"name": "POS-80", "PortaNome": "USB001"},
		{"name": "Microsoft XPS Document Writer", "PortaNome": "PORTPROMPT:"},
	}
	kept, dropped := agent.FilterAndRankDetails(raw)
	if len(kept) != 1 || kept[0]["name"] != "POS-80" {
		t.Fatalf("kept: %#v", kept)
	}
	if len(dropped) != 1 || dropped[0] != "Microsoft XPS Document Writer" {
		t.Fatalf("dropped: %v", dropped)
	}
	kept2, dropped2 := agent.FilterAndRankDetails([]map[string]string{{"name": "Só uma"}})
	if len(kept2) != 1 || len(dropped2) != 0 {
		t.Fatalf("sem descarte não ganha dropped: %v", dropped2)
	}
}

// FilterAndRank delega no FilterAndRankDetails: um filtro só — o "dropped"
// do log nunca diverge do snapshot que o report leva.
func TestFilterAndRankDelegaNoDetails(t *testing.T) {
	raw := []map[string]string{
		{"name": "Tomate MDK-080", "PortaNome": "USB001", "driver": "printqueue.inf"},
		{"name": "Microsoft Print to PDF", "PortaNome": "PORTPROMPT:", "driver": "pdf"},
		{"name": "  ", "PortaNome": "x", "driver": "y"}, // sem nome → descartada
		nil,
		{"name": "HP DeskJet", "PortaNome": "USB002", "driver": "hp"},
	}
	kept, dropped := agent.FilterAndRankDetails(raw)
	viaSimples := agent.FilterAndRank(raw)
	if len(viaSimples) != len(kept) {
		t.Fatalf("FilterAndRank divergiu do Details: %d vs %d", len(viaSimples), len(kept))
	}
	for i := range kept {
		if viaSimples[i]["name"] != kept[i]["name"] {
			t.Fatalf("ordem/conteúdo divergiu na %d: %q vs %q", i, viaSimples[i]["name"], kept[i]["name"])
		}
	}
	// Descartadas na ordem de varredura: a PDF e a sem-nome (nil não vira linha).
	if len(dropped) != 2 || dropped[0] != "Microsoft Print to PDF" || dropped[1] != "(sem nome)" {
		t.Fatalf("dropped: %v", dropped)
	}
	// Térmica primeiro (score: usb+mdk > nada) e o teto de 50 não afeta 2.
	if len(kept) != 2 || kept[0]["name"] != "Tomate MDK-080" || kept[1]["name"] != "HP DeskJet" {
		t.Fatalf("kept: %v", kept)
	}
}
