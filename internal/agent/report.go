package agent

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
)

// Report de impressoras do boot (epic §4 passo 2 + Rails §3 passo 2).
// Todas as chaves/valores são strings; chaves cravadas do contrato:
// name, PortaNome (nome da porta no OS), Host (IP da porta TCP/IP) e
// Porta (porta TCP numérica como string). O agente FILTRA impressoras de
// arquivo/PDF/XPS e ordena por heurística de térmica ANTES do teto de 50 —
// senão o corte pode excluir justamente a térmica (epic §4).
const (
	maxReportPrinters = 50
)

// ---------- filtragem e ordem ----------

var virtualPatterns = []string{
	"portprompt:", // porta virtual do Windows (imprime em arquivo)
	"file:",       // idem
	"pdf",         // Microsoft Print to PDF e congêneres — nada térmico tem PDF no nome
	"xps",
	"onenote",
}

// isVirtualPrinter: impressoras de arquivo/PDF/XPS não interessam ao painel.
func isVirtualPrinter(entry map[string]string) bool {
	name := strings.ToLower(entry["name"])
	driver := strings.ToLower(entry["driver"])
	port := strings.ToLower(entry["PortaNome"])
	comb := name + "\n" + driver + "\n" + port
	for _, pat := range virtualPatterns {
		if strings.Contains(comb, pat) {
			return true
		}
	}
	return false
}

// thermalScore: quanto maior, mais a impressora parece térmica (RAW em rede
// ou USB — o par típico de comanda). Ordenar por isso antes do teto de 50
// garante que a térmica não sai do corte quando o balcão tem 60 impressoras.
func thermalScore(entry map[string]string) int {
	score := 0
	if entry["Host"] != "" {
		score += 3 // porta TCP/IP RAW — o writer network fala com ela direto
	}
	if strings.Contains(strings.ToLower(entry["PortaNome"]), "usb") {
		score += 2
	}
	comb := strings.ToLower(entry["name"] + " " + entry["driver"])
	for _, pat := range []string{"pos", "thermal", "térmica", "receipt", "mdk", "tm-", "tanca", "bematech", "elgin", "daruma", "esc/pos", "escpos", "sweda", "epson tm"} {
		if strings.Contains(comb, pat) {
			score += 2
			break
		}
	}
	return score
}

// FilterAndRank: filtra virtuais, ordena térmica primeiro (score desc,
// empate alfabético por name) e corta no teto de 50. Delega no
// FilterAndRankDetails — o filtro é UM só, os motivos do log nunca
// divergem do que o report leva.
func FilterAndRank(entries []map[string]string) []map[string]string {
	kept, _ := FilterAndRankDetails(entries)
	return kept
}

// FilterAndRankDetails é o filtro ÚNICO (FilterAndRank delega aqui): além
// do snapshot que vai no report, lista o nome do que foi descartado
// (virtuais e entradas sem nome) — o log de boot precisa explicar a
// diferença entre coletado e enviado, senão "0 coletada(s)" não diz se
// faltou driver ou sobrou filtro. Ordena térmica primeiro (score desc,
// empate alfabético por name) e corta no teto de 50.
func FilterAndRankDetails(entries []map[string]string) (kept []map[string]string, dropped []string) {
	kept = make([]map[string]string, 0, len(entries))
	for _, e := range entries {
		if e == nil {
			continue
		}
		if strings.TrimSpace(e["name"]) == "" {
			dropped = append(dropped, "(sem nome)")
			continue // entrada sem name não sobrevive no servidor de qualquer forma
		}
		if isVirtualPrinter(e) {
			dropped = append(dropped, e["name"])
			continue
		}
		kept = append(kept, e)
	}
	sort.SliceStable(kept, func(i, j int) bool {
		si, sj := thermalScore(kept[i]), thermalScore(kept[j])
		if si != sj {
			return si > sj
		}
		return kept[i]["name"] < kept[j]["name"]
	})
	if len(kept) > maxReportPrinters {
		kept = kept[:maxReportPrinters]
	}
	return kept, dropped
}

// ---------- coleta do boot (resumo com evidências) ----------

// DiscoveryResume resume UMA passada de coleta: o que o OS entregou, o que
// sobreviveu ao filtro e as evidências do caminho (falha de API, contagens,
// dica de ação). Nada silencioso: "0 impressoras" chega no log com motivo —
// suporte precisa distinguir "driver não instalado" de "spooler/CUPS morto".
type DiscoveryResume struct {
	RawCount int                 // coletado do OS, antes do filtro
	Kept     []map[string]string // após filtro+ordem — é o que vai no report
	Dropped  []string            // nomes descartados pelo filtro
	Notes    []string            // evidências do OS/dicas, uma linha cada
}

// CollectPrinters é o passo de coleta do boot: descoberta do OS → filtro →
// ordem, com o resumo do que houve para o log/debug.
func CollectPrinters(ctx context.Context) DiscoveryResume {
	raw, notes := discoverPrinters(ctx)
	return BuildDiscoveryResume(raw, notes)
}

// BuildDiscoveryResume junta descoberta + filtro num resumo único — puro,
// testável em qualquer OS.
func BuildDiscoveryResume(raw []map[string]string, notes []string) DiscoveryResume {
	kept, dropped := FilterAndRankDetails(raw)
	res := DiscoveryResume{RawCount: len(raw), Kept: kept, Dropped: dropped, Notes: notes}
	switch {
	case len(raw) == 0:
		// Zero impressoras — o motivo específico já veio nas notas do OS
		// (sem driver, spooler parado, CUPS ausente…); a dica de ação é a
		// mesma nos dois casos e lembra que o report só acontece no boot.
		res.Notes = append(res.Notes,
			"nenhuma impressora encontrada no sistema — instale o driver da térmica e reinicie o agente (o report de impressoras só acontece no boot)")
	case len(dropped) > 0:
		res.Notes = append(res.Notes,
			fmt.Sprintf("filtro do report descartou %d entrada(s): %s", len(dropped), clampLine(strings.Join(dropped, ", "), 300)))
	}
	return res
}

// clampLine corta em ~max runas (nomes do OS não podem quebrar o formato do
// log; o buffer de debug trunca por runa também).
func clampLine(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "…"
}

// ---------- CUPS (Linux/macOS) ----------

var (
	lpstatNameRe = regexp.MustCompile(`^printer (\S+)`)
	lpstatDevRe  = regexp.MustCompile(`^device for (\S+): (\S+)\s*$`)
)

// ParseLpstatP extrai os nomes de fila de `lpstat -p`
// ("printer POS80 is idle.  enabled since ...").
func ParseLpstatP(out string) []string {
	var names []string
	seen := map[string]bool{}
	for _, line := range strings.Split(out, "\n") {
		if m := lpstatNameRe.FindStringSubmatch(line); m != nil && !seen[m[1]] {
			seen[m[1]] = true
			names = append(names, m[1])
		}
	}
	return names
}

// ParseLpstatV extrai nome→device URI de `lpstat -v`
// ("device for POS80: socket://192.168.0.50:9100").
func ParseLpstatV(out string) map[string]string {
	uris := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		if m := lpstatDevRe.FindStringSubmatch(line); m != nil {
			uris[m[1]] = m[2]
		}
	}
	return uris
}

// CupsEntries monta as entradas do report a partir do lpstat: Host/Porta
// saem do URI socket://host:port (porta desconhecida → omitir Porta —
// epic §4); PortaNome é o nome da fila (a identidade de porta do CUPS —
// é ela que o writer usb usa no lp -d).
func CupsEntries(names []string, uris map[string]string) []map[string]string {
	entries := make([]map[string]string, 0, len(names))
	for _, name := range names {
		uri := uris[name]
		if uri == "" {
			// Fila sem device visível ainda entra (o painel exibe o nome).
			entries = append(entries, map[string]string{"name": name})
			continue
		}
		e := map[string]string{"name": name, "PortaNome": name}
		switch {
		case strings.HasPrefix(uri, "socket://"):
			rest := strings.TrimPrefix(uri, "socket://")
			if qi := strings.IndexByte(rest, '?'); qi >= 0 {
				rest = rest[:qi]
			}
			host, port, hasPort := splitHostPort(rest)
			if host != "" {
				e["Host"] = host
			}
			if hasPort {
				e["Porta"] = port
			}
		case strings.HasPrefix(uri, "usb://"), strings.HasPrefix(uri, "dev://"):
			// USB do CUPS: Host/Porta não se aplicam; o nome da fila basta.
		default:
			// ipp://, http://, lpadmin apontando pro socket da ponte local etc.
			if strings.HasPrefix(uri, "ipp://") || strings.HasPrefix(uri, "http://") {
				rest := uri[strings.Index(uri, "://")+3:]
				if qi := strings.IndexByte(rest, '?'); qi >= 0 {
					rest = rest[:qi]
				}
				rest = strings.TrimSuffix(rest, "/")
				if hi := strings.IndexByte(rest, '/'); hi >= 0 {
					rest = rest[:hi]
				}
				if host, port, hasPort := splitHostPort(rest); host != "" && hasPort {
					e["Host"] = host
					e["Porta"] = port
				}
			}
		}
		entries = append(entries, e)
	}
	return entries
}

func splitHostPort(s string) (host, port string, ok bool) {
	if i := strings.LastIndexByte(s, ':'); i >= 0 && !strings.Contains(s[i:], "]") {
		p := s[i+1:]
		if n, err := strconv.Atoi(p); err == nil && n > 0 && n < 65536 {
			return s[:i], p, true
		}
		return s[:i], "", false
	}
	return s, "", false
}

// ---------- Windows ----------

// WinPrinterRow é o que o EnumPrinters (level 2) coleta — a conversão para
// entradas do report é pura (testável em qualquer OS).
type WinPrinterRow struct {
	Name       string
	PortName   string
	DriverName string
	ShareName  string
	Location   string
	Comment    string
	Status     uint32
}

// WinPortRow é o que o EnumPorts (level 2) coleta.
type WinPortRow struct {
	Name        string
	Description string
}

// TCPHostLookup resolve Host/Porta de uma porta TCP/IP do Windows —
// registry do monitor "Standard TCP/IP Port" com fallback pro nome da porta
// (IP_192.168.0.50). Nil/retorno (","","") = não é porta TCP.
type TCPHostLookup func(portName string) (host, port string)

// WindowsEntries converte as linhas coletadas nas entradas do report
// (chaves cravadas do contrato). Impressoras compartilhadas (porta
// \\servidor\imp) entram com a PortaNome crua, sem Host/Porta.
func WindowsEntries(rows []WinPrinterRow, ports []WinPortRow, lookup TCPHostLookup) []map[string]string {
	entries := make([]map[string]string, 0, len(rows))
	for _, r := range rows {
		e := map[string]string{
			"name":      r.Name,
			"PortaNome": r.PortName,
		}
		if r.DriverName != "" {
			e["driver"] = r.DriverName
		}
		if r.Status != 0 {
			if s := PrinterStatusText(r.Status); s != "" {
				e["Status"] = s
			}
		}
		if r.ShareName != "" {
			e["ShareName"] = r.ShareName
		}
		if r.Location != "" {
			e["Location"] = r.Location
		}
		if r.Comment != "" {
			e["Comment"] = r.Comment
		}
		if lookup != nil && !strings.HasPrefix(r.PortName, `\`) {
			if host, port := lookup(r.PortName); host != "" {
				e["Host"] = host
				if port != "" {
					e["Porta"] = port
				}
			}
		}
		entries = append(entries, e)
	}
	return entries
}

// TCPLookupFromPorts monta o lookup a partir das portas do EnumPorts: só
// porta descrita como TCP/IP (ou nomeada IP_*) é candidata a Host/Porta —
// mesmo que o EnumPorts a tenha perdido (o nome IP_* é evidência suficiente).
func TCPLookupFromPorts(ports []WinPortRow, resolve func(portName string) (host, port string)) TCPHostLookup {
	tcp := map[string]bool{}
	for _, p := range ports {
		d := strings.ToLower(p.Description)
		if strings.Contains(d, "tcp/ip") || strings.Contains(strings.ToLower(p.Name), "ip_") {
			tcp[p.Name] = true
		}
	}
	return func(portName string) (string, string) {
		low := strings.ToLower(portName)
		if !tcp[portName] && !strings.HasPrefix(low, "ip_") {
			return "", ""
		}
		if resolve != nil {
			if h, p := resolve(portName); h != "" {
				return h, p
			}
		}
		// Fallback: nome padrão do monitor TCP/IP do Windows — IP_<host>.
		if strings.HasPrefix(low, "ip_") && len(portName) > 3 {
			host := portName[3:]
			if host != "" {
				return host, "9100"
			}
		}
		return "", ""
	}
}

// PrinterStatusText: os bits de PRINTER_STATUS_* que importam para o
// suporte ler o painel (o resto vira "0x…" cru).
func PrinterStatusText(status uint32) string {
	parts := []string{}
	add := func(bit uint32, txt string) {
		if status&bit != 0 {
			parts = append(parts, txt)
		}
	}
	// Valores do winspool.h do SDK (conferidos no header — bit errado aqui
	// vira diagnóstico errado no painel). IO_ACTIVE (0x100) fica de fora de
	// propósito: atividade normal de I/O não é estado de problema.
	add(1, "pausada")                           // PRINTER_STATUS_PAUSED
	add(2, "erro")                              // PRINTER_STATUS_ERROR
	add(4, "excluindo")                         // PRINTER_STATUS_PENDING_DELETION
	add(8, "papel travado")                     // PRINTER_STATUS_PAPER_JAM
	add(16, "sem papel")                        // PRINTER_STATUS_PAPER_OUT
	add(32, "alimentação manual")               // PRINTER_STATUS_MANUAL_FEED
	add(64, "problema no papel")                // PRINTER_STATUS_PAPER_PROBLEM
	add(128, "offline")                         // PRINTER_STATUS_OFFLINE
	add(512, "ocupada")                         // PRINTER_STATUS_BUSY
	add(1024, "imprimindo")                     // PRINTER_STATUS_PRINTING
	add(2048, "bandeja de saída cheia")         // PRINTER_STATUS_OUTPUT_BIN_FULL
	add(4096, "indisponível")                   // PRINTER_STATUS_NOT_AVAILABLE
	add(8192, "aguardando")                     // PRINTER_STATUS_WAITING
	add(16384, "processando")                   // PRINTER_STATUS_PROCESSING
	add(32768, "inicializando")                 // PRINTER_STATUS_INITIALIZING
	add(65536, "aquecendo")                     // PRINTER_STATUS_WARMING_UP
	add(131072, "toner baixo")                  // PRINTER_STATUS_TONER_LOW
	add(262144, "sem toner")                    // PRINTER_STATUS_NO_TONER
	add(524288, "falha na página")              // PRINTER_STATUS_PAGE_PUNT
	add(1048576, "intervenção do usuário")      // PRINTER_STATUS_USER_INTERVENTION
	add(2097152, "sem memória")                 // PRINTER_STATUS_OUT_OF_MEMORY
	add(4194304, "tampa aberta")                // PRINTER_STATUS_DOOR_OPEN
	add(8388608, "servidor de impressão sumiu") // PRINTER_STATUS_SERVER_UNKNOWN
	add(16777216, "economia de energia")        // PRINTER_STATUS_POWER_SAVE
	if len(parts) == 0 {
		return fmt.Sprintf("0x%x", status)
	}
	return strings.Join(parts, "+")
}

// BuildReportBody monta o corpo do POST /impressora/printers: snapshot +
// telemetria (epic §7.1 — Rails grava agent_version hoje; os demais campos
// são aditivos e ignorados pelo servidor atual sem erro).
func BuildReportBody(printers []map[string]string) map[string]any {
	hostname, _ := os.Hostname()
	return map[string]any{
		"agent_version": Version,
		"os":            runtime.GOOS,
		"arch":          runtime.GOARCH,
		"hostname":      hostname,
		"printers":      printers,
	}
}
