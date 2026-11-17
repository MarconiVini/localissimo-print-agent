//go:build windows

package agent

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unsafe"
)

// Windows (epic §2): descoberta via EnumPrinters (level 2 — nome, driver,
// porta, status) + EnumPorts (level 2 — para resolver Host/Porta das portas
// TCP/IP) + registry do monitor "Standard TCP/IP Port" (host e portNumber
// de verdade). Writer usb via spooler RAW:
// OpenPrinter → StartDocPrinter(level 1, "RAW") → WritePrinter →
// EndDocPrinter → ClosePrinter.

var (
	winspool             = syscall.NewLazyDLL("winspool.drv")
	procEnumPrintersW    = winspool.NewProc("EnumPrintersW")
	procEnumPortsW       = winspool.NewProc("EnumPortsW")
	procOpenPrinterW     = winspool.NewProc("OpenPrinterW")
	procStartDocPrinterW = winspool.NewProc("StartDocPrinterW")
	procWritePrinter     = winspool.NewProc("WritePrinter")
	procEndDocPrinter    = winspool.NewProc("EndDocPrinter")
	procClosePrinter     = winspool.NewProc("ClosePrinter")

	// Registry (stdlib não expõe Reg* — vai direto no advapi32).
	advapi32             = syscall.NewLazyDLL("advapi32.dll")
	procRegOpenKeyExW    = advapi32.NewProc("RegOpenKeyExW")
	procRegQueryValueExW = advapi32.NewProc("RegQueryValueExW")
	procRegCloseKeyW     = advapi32.NewProc("RegCloseKey")
)

const regSAMRead = 0x20019 // KEY_READ

const (
	printerEnumLocal       = 0x2
	printerEnumConnections = 0x4
	errInsufficientBuffer  = syscall.Errno(122)
)

// Layouts x64/ARM64 (mesmos tamanhos — ponteiros de 8 bytes).
//
// printerInfo2W espelha a PRINTER_INFO_2W DE CABEÇA: a struct real NÃO tem
// campo Flags (o Flags é da PRINTER_INFO_1) e a ordem é pPrinterName,
// pShareName, pPortName, pDriverName… — um Flags inventado no início +
// ShareName/PortName trocados deslocavam todos os campos e o stride do
// array (128 inventados vs 120 reais): nomes/portas liam slots errados.
type printerInfo2W struct {
	PrinterName        uintptr
	ShareName          uintptr
	PortName           uintptr
	DriverName         uintptr
	Comment            uintptr
	Location           uintptr
	DevMode            uintptr
	SepFile            uintptr
	PrintProcessor     uintptr
	Datatype           uintptr
	Parameters         uintptr
	SecurityDescriptor uintptr
	Attributes         uint32
	Priority           uint32
	DefaultPriority    uint32
	Status             uint32
	CJobs              uint32
	AveragePPM         uint32
}

// Layout level 4 (nomes crus — o contraste diagnóstico do level 2).
type printerInfo4W struct {
	PrinterName uintptr
	ServerName  uintptr
	Attributes  uint32
	_           uint32
}

type portInfo2W struct {
	PortName    uintptr
	MonitorName uintptr
	Description uintptr
	PortType    uint32
	Reserved    uint32
}

type docInfo1W struct {
	DocName    uintptr
	OutputFile uintptr
	Datatype   uintptr
}

// Sanity de layout: se o Go mudar alinhamento, melhor explodir no boot do
// que mandar report corrompido.
func checkWinLayouts() error {
	if unsafe.Sizeof(printerInfo2W{}) != 120 {
		return fmt.Errorf("layout printerInfo2W inesperado: %d", unsafe.Sizeof(printerInfo2W{}))
	}
	if unsafe.Sizeof(portInfo2W{}) != 32 {
		return fmt.Errorf("layout portInfo2W inesperado: %d", unsafe.Sizeof(portInfo2W{}))
	}
	if unsafe.Sizeof(docInfo1W{}) != 24 {
		return fmt.Errorf("layout docInfo1W inesperado: %d", unsafe.Sizeof(docInfo1W{}))
	}
	if unsafe.Sizeof(printerInfo4W{}) != 24 {
		return fmt.Errorf("layout printerInfo4W inesperado: %d", unsafe.Sizeof(printerInfo4W{}))
	}
	return nil
}

// utf16Field lê a string UTF-16 (NUL-terminada) apontada por ptr. Os campos
// dos layouts Enum* são PONTEIROS ABSOLUTOS de processo (apontam pra dentro
// deste mesmo buffer) — o offset real é ptr - base do buffer. Usar o valor
// cru como índice dá sempre fora dos limites (endereço de heap ≫ len(buf))
// e todo campo volta "" — foi o bug do "4 impressora(s):  (porta )" no log.
func utf16Field(buf []byte, ptr uintptr) string {
	if ptr == 0 || len(buf) == 0 {
		return ""
	}
	base := uintptr(unsafe.Pointer(&buf[0]))
	if ptr < base || ptr >= base+uintptr(len(buf)) {
		return "" // ponteiro fora do buffer do Enum* — nada nossa pra ler
	}
	off := int(ptr - base)
	u16 := unsafe.Slice((*uint16)(unsafe.Pointer(&buf[off])), (len(buf)-off)/2)
	return syscall.UTF16ToString(u16)
}

// enumDuasChamadas roda o padrão de duas chamadas do winspool (sonda de
// tamanho → buffer realocado). Os closures montam os argumentos COMPLETOS
// da API, contagem exata, sem prefixo remontável: EnumPrintersW leva 7 args
// e EnumPortsW 6 — foi errar um nesse remonte (prefixo faltando deslocava
// tudo, o level caía no lugar do Name=0x6 e o winspool dereferenciou) que
// derrubou o agente no Windows com SEGV sem log nenhum (0.1.3 "nem abre").
func enumDuasChamadas(
	proc *syscall.LazyProc,
	sonda func(needed, returned *uint32) []uintptr,
	comBuffer func(buf []byte, needed, returned *uint32) []uintptr,
) ([]byte, uint32, error) {
	var needed, returned uint32
	// 1ª chamada com buffer nulo só para descobrir o tamanho
	// (retorna FALSE com ERROR_INSUFFICIENT_BUFFER quando há dados).
	r1, _, callErr := proc.Call(sonda(&needed, &returned)...)
	if needed == 0 {
		if r1 == 0 {
			// FALSE sem buffer NÃO é "zero impressoras": é falha de verdade
			// (spooler parado, acesso negado…) — sobe com o errno do Windows
			// pra não virar "0 coletada(s)" sem motivo no log.
			return nil, 0, fmt.Errorf("%s: %v", proc.Name, callErr)
		}
		return nil, 0, nil // TRUE com zero elementos: lista vazia de verdade
	}
	// 2ª chamada com o buffer. ERROR_INSUFFICIENT_BUFFER AQUI = a estimativa
	// cresceu entre as chamadas (fila mudou no meio — típico no boot com o
	// spooler ainda assentando): realoca com o tamanho novo e repete.
	for tentativa := 0; ; tentativa++ {
		buf := make([]byte, needed)
		r2, _, callErr2 := proc.Call(comBuffer(buf, &needed, &returned)...)
		if r2 != 0 {
			return buf, returned, nil
		}
		if callErr2 != errInsufficientBuffer || tentativa >= 2 {
			return nil, 0, fmt.Errorf("%s: %v", proc.Name, callErr2)
		}
		// needed agora tem o tamanho atualizado; a próxima iteração realoca.
	}
}

// enumPrintersW — EnumPrintersW(Flags, Name, Level, pPrinter, cbBuf,
// pcbNeeded, pcReturned): 7 args, nesta ordem. Name=NULL = servidor local
// (as flags LOCAL|CONNECTIONS cobrem instaladas + conectadas por rede).
func enumPrintersW(flags uintptr, level uint32) ([]byte, uint32, error) {
	return enumDuasChamadas(procEnumPrintersW,
		func(needed, returned *uint32) []uintptr {
			return []uintptr{flags, 0, uintptr(level), 0, 0,
				uintptr(unsafe.Pointer(needed)), uintptr(unsafe.Pointer(returned))}
		},
		func(buf []byte, needed, returned *uint32) []uintptr {
			return []uintptr{flags, 0, uintptr(level),
				uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)),
				uintptr(unsafe.Pointer(needed)), uintptr(unsafe.Pointer(returned))}
		})
}

// enumPortsW — EnumPortsW(pName, Level, pPorts, cbBuf, pcbNeeded,
// pcReturned): 6 args, nesta ordem. pName=NULL = portas locais.
func enumPortsW(level uint32) ([]byte, uint32, error) {
	return enumDuasChamadas(procEnumPortsW,
		func(needed, returned *uint32) []uintptr {
			return []uintptr{0, uintptr(level), 0, 0,
				uintptr(unsafe.Pointer(needed)), uintptr(unsafe.Pointer(returned))}
		},
		func(buf []byte, needed, returned *uint32) []uintptr {
			return []uintptr{0, uintptr(level),
				uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)),
				uintptr(unsafe.Pointer(needed)), uintptr(unsafe.Pointer(returned))}
		})
}

// discoverPrintersWindows coleta spooler + portas. Falha de coleta NÃO
// trava o boot: report vazio, mas o motivo e o que o Windows enxergou vão
// nas notes (uma linha cada) — é o que distingue "driver não instalado" de
// "spooler morto" no agent.log do balcão.
func discoverPrintersWindows() ([]map[string]string, []string) {
	if err := checkWinLayouts(); err != nil {
		return nil, []string{fmt.Sprintf("coleta do Windows abortada: %v", err)}
	}
	pbuf, n, err := enumPrintersW(printerEnumLocal|printerEnumConnections, 2)
	if err != nil {
		return nil, []string{
			fmt.Sprintf("EnumPrinters falhou: %v — o serviço spooler do Windows está rodando? (services.msc → Spooler de Impressão)", err),
		}
	}
	rows := make([]WinPrinterRow, 0, n)
	for i := uint32(0); i < n; i++ {
		pi := (*printerInfo2W)(unsafe.Pointer(&pbuf[uintptr(i)*unsafe.Sizeof(printerInfo2W{})]))
		rows = append(rows, WinPrinterRow{
			Name:       utf16Field(pbuf, pi.PrinterName),
			PortName:   utf16Field(pbuf, pi.PortName),
			DriverName: utf16Field(pbuf, pi.DriverName),
			ShareName:  utf16Field(pbuf, pi.ShareName),
			Location:   utf16Field(pbuf, pi.Location),
			Comment:    utf16Field(pbuf, pi.Comment),
			Status:     pi.Status,
		})
	}
	// Level 2 pode vir inteiro SEM nomes (o Wine 10 devolve PrinterName=NULL
	// nas filas CUPS — o nome real só aparece no level 4, dumpado no diag).
	// Entrada sem nome não reporta nada útil: conta só as com nome e trata
	// zero como vazio.
	comNome := 0
	for _, r := range rows {
		if strings.TrimSpace(r.Name) != "" {
			comNome++
		}
	}
	if comNome == 0 {
		// Zero (ou só sem-nome) no level 2 com spooler vivo é caso de canto
		// (vi um Windows aí imprimir de teste e o level 2 devolver vazio, e o
		// Wine fazer isso de novo). O level 4 — caminho mais leve, só nomes —
		// serve de contraste: se ele enxerga, reporta os nomes mesmo (o
		// writer usb só precisa do nome via OpenPrinter; perde porta/driver,
		// mas a impressora chega no painel).
		resumo2 := "zero entrada(s)"
		if len(rows) > 0 {
			resumo2 = fmt.Sprintf("%d entrada(s) sem nome (o que veio: %s)", len(rows), clampLine(summarizeWindowsRows(rows), 200))
		}
		b4, n4, e4 := enumPrintersW(printerEnumLocal|printerEnumConnections, 4)
		if e4 == nil && n4 > 0 {
			nomes := make([]string, 0, n4)
			rows4 := make([]WinPrinterRow, 0, n4)
			for i := uint32(0); i < n4; i++ {
				pi4 := (*printerInfo4W)(unsafe.Pointer(&b4[uintptr(i)*unsafe.Sizeof(printerInfo4W{})]))
				nome := utf16Field(b4, pi4.PrinterName)
				nomes = append(nomes, nome)
				rows4 = append(rows4, WinPrinterRow{Name: nome})
			}
			notes := []string{fmt.Sprintf(
				"spooler: level 2 veio com %s mas level 4 vê %d impressora(s): %s — reportando nomes sem porta/driver (caso de canto do EnumPrinters level 2)",
				resumo2, n4, clampLine(strings.Join(nomes, ", "), 300))}
			return WindowsEntries(rows4, nil, nil), notes
		}
		hint := fmt.Sprintf("spooler: level 2 veio com %s e o level 4 não deu nome — instale o driver da térmica (ela precisa aparecer em Configurações > Bluetooth e impressoras) e reinicie o agente", resumo2)
		if e4 != nil {
			hint += fmt.Sprintf(" (level 4 falhou: %v)", e4)
		}
		return nil, []string{hint}
	}
	notes := []string{fmt.Sprintf("spooler: %d impressora(s): %s", len(rows), clampLine(summarizeWindowsRows(rows), 300))}

	ports := []WinPortRow{}
	if obuf, on, oerr := enumPortsW(2); oerr == nil {
		for i := uint32(0); i < on; i++ {
			po := (*portInfo2W)(unsafe.Pointer(&obuf[uintptr(i)*unsafe.Sizeof(portInfo2W{})]))
			ports = append(ports, WinPortRow{
				Name:        utf16Field(obuf, po.PortName),
				Description: utf16Field(obuf, po.Description),
			})
		}
		notes = append(notes, fmt.Sprintf("EnumPorts: %d porta(s)", on))
	} else {
		// EnumPorts falho não derruba o report — só perde Host/Porta.
		notes = append(notes, fmt.Sprintf("EnumPorts falhou: %v — Host/Porta das portas TCP/IP ficam de fora", oerr))
	}

	lookup := TCPLookupFromPorts(ports, lookupTCPPort)
	return WindowsEntries(rows, ports, lookup), notes
}

// summarizeWindowsRows resume as linhas do spooler em uma linha — é a
// evidência do "0 coletada(s)": o suporte vê na hora o que o Windows
// enxerga (e se a térmica está entre elas, com qual porta/driver).
func summarizeWindowsRows(rows []WinPrinterRow) string {
	parts := make([]string, 0, len(rows))
	for _, r := range rows {
		p := r.Name + " (porta " + r.PortName
		if r.DriverName != "" {
			p += ", driver " + r.DriverName
		}
		p += ")"
		parts = append(parts, p)
	}
	return strings.Join(parts, "; ")
}

// lookupTCPPort resolve host/portNumber da porta TCP/IP no registry do
// monitor "Standard TCP/IP Port". Falha → ("","") (o caller faz fallback
// pelo nome da porta IP_*).
func lookupTCPPort(portName string) (string, string) {
	path := `SYSTEM\CurrentControlSet\Control\Print\Monitors\Standard TCP/IP Port\Ports\` + portName
	p16, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return "", ""
	}
	var h uintptr
	if r, _, _ := procRegOpenKeyExW.Call(uintptr(syscall.HKEY_LOCAL_MACHINE),
		uintptr(unsafe.Pointer(p16)), 0, regSAMRead, uintptr(unsafe.Pointer(&h))); r != 0 {
		return "", ""
	}
	defer procRegCloseKeyW.Call(h)
	host := regQueryString(h, "HostName")
	if host == "" {
		host = regQueryString(h, "IPAddress")
	}
	if host == "" {
		return "", ""
	}
	port := regQueryPort(h, "PortNumber")
	return host, port
}

// REG_* do winnt.h (constantes que o syscall não expõe).
const (
	regSZ       = 1
	regExpandSZ = 2
	regDWORD    = 4
)

func regQueryString(h uintptr, name string) string {
	n16, err := syscall.UTF16PtrFromString(name)
	if err != nil {
		return ""
	}
	var buf [512]uint16
	kind := uint32(0)
	size := uint32(len(buf)) * 2
	r, _, _ := procRegQueryValueExW.Call(h, uintptr(unsafe.Pointer(n16)), 0,
		uintptr(unsafe.Pointer(&kind)), uintptr(unsafe.Pointer(&buf[0])),
		uintptr(unsafe.Pointer(&size)))
	if r != 0 || (kind != regSZ && kind != regExpandSZ) {
		return ""
	}
	return syscall.UTF16ToString(buf[:size/2])
}

func regQueryPort(h uintptr, name string) string {
	n16, err := syscall.UTF16PtrFromString(name)
	if err != nil {
		return ""
	}
	kind := uint32(0)
	var dw uint32
	size := uint32(4)
	r, _, _ := procRegQueryValueExW.Call(h, uintptr(unsafe.Pointer(n16)), 0,
		uintptr(unsafe.Pointer(&kind)), uintptr(unsafe.Pointer(&dw)),
		uintptr(unsafe.Pointer(&size)))
	if r != 0 {
		return ""
	}
	switch kind {
	case regDWORD:
		if dw > 0 && dw < 65536 {
			return strconv.Itoa(int(dw))
		}
	case regSZ:
		var buf [16]uint16
		sz := uint32(len(buf)) * 2
		if r2, _, _ := procRegQueryValueExW.Call(h, uintptr(unsafe.Pointer(n16)), 0,
			uintptr(unsafe.Pointer(&kind)), uintptr(unsafe.Pointer(&buf[0])),
			uintptr(unsafe.Pointer(&sz))); r2 == 0 && (kind == regSZ || kind == regExpandSZ) {
			if p, perr := strconv.Atoi(syscall.UTF16ToString(buf[:sz/2])); perr == nil && p > 0 && p < 65536 {
				return strconv.Itoa(p)
			}
		}
	}
	return ""
}

// writeUSBPlatform despacha o writer usb do OS (matriz epic §2).
// writeUSBPlatform empacota o write do spooler com teto de tempo: syscall
// de spooler não tem deadline e um port monitor travado (USB meio-morto,
// driver wedged) bloquearia o loop do agente PARA SEMPRE — sem poll, sem
// heartbeat, device vermelho no painel, e nem Ctrl+C desenrola syscall
// bloqueada no Windows. Estourou → o Round segue (job fica pending e
// reimprime — at-least-once). O goroutine abandonado não tem como ser
// morto: se o write sair depois do timeout, a comanda pode sair em dobro —
// mesmo tradeoff de ack falho, e infinitamente melhor que o agente morto.
func writeUSBPlatform(ctx context.Context, printerName string, payload []byte) error {
	done := make(chan error, 1) // bufferizado: o goroutine abandonado não vaza
	go func() { done <- writeUSBWindows(printerName, payload) }()
	timer := time.NewTimer(usbWriteTimeout)
	defer timer.Stop()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return fmt.Errorf("write usb cancelado (encerramento) — spooler segue em background: %w", ctx.Err())
	case <-timer.C:
		return fmt.Errorf("spooler sem resposta em %s — write abandonado, job segue pending (reimprime; se sair depois, sai em dobro)", usbWriteTimeout)
	}
}

// writeUSBWindows imprime via spooler RAW pelo nome EXATO do spooler
// (printer_name da config) — tudo-ou-nada.
func writeUSBWindows(printerName string, payload []byte) error {
	if strings.TrimSpace(printerName) == "" {
		return errors.New("conexão usb sem printer_name na config")
	}
	if err := checkWinLayouts(); err != nil {
		return err
	}
	name16, err := syscall.UTF16PtrFromString(printerName)
	if err != nil {
		return fmt.Errorf("printer_name inválido: %w", err)
	}
	var h syscall.Handle
	if r, _, callErr := procOpenPrinterW.Call(uintptr(unsafe.Pointer(name16)),
		uintptr(unsafe.Pointer(&h)), 0); r == 0 {
		return fmt.Errorf("OpenPrinter(%q): %v", printerName, callErr)
	}
	defer procClosePrinter.Call(uintptr(h))

	// As strings do DOC_INFO_1W precisam estar VIVAS até o StartDocPrinter
	// retornar: uintptr(unsafe.Pointer(...)) guardado em campo de struct não
	// é enxergado pelo GC — sem os ponteiros Go nos locais + KeepAlive, um
	// ciclo de GC no meio liberaria a memória e o spooler leria lixo
	// (datatype corrompido no job — bug intermitente, raro e cruel).
	docName16 := mustUTF16("Localissimo comanda")
	datatype16 := mustUTF16("RAW") // DataType RAW — passa direto pela fila
	doc := docInfo1W{
		DocName:    uintptr(unsafe.Pointer(docName16)),
		OutputFile: 0,
		Datatype:   uintptr(unsafe.Pointer(datatype16)),
	}
	if r, _, callErr := procStartDocPrinterW.Call(uintptr(h), 1, uintptr(unsafe.Pointer(&doc))); r == 0 || r == ^uintptr(0) {
		return fmt.Errorf("StartDocPrinter(%q): %v", printerName, callErr)
	}
	defer func() {
		runtime.KeepAlive(docName16)
		runtime.KeepAlive(datatype16)
	}()
	written := uint32(0)
	for written < uint32(len(payload)) {
		var n uint32
		chunk := payload[written:]
		if r, _, callErr := procWritePrinter.Call(uintptr(h),
			uintptr(unsafe.Pointer(&chunk[0])), uintptr(len(chunk)),
			uintptr(unsafe.Pointer(&n))); r == 0 {
			return fmt.Errorf("WritePrinter(%q): %d/%d bytes: %v", printerName, written, len(payload), callErr)
		}
		if n == 0 {
			return fmt.Errorf("WritePrinter(%q): progresso nulo em %d/%d bytes", printerName, written, len(payload))
		}
		written += n
	}
	if r, _, callErr := procEndDocPrinter.Call(uintptr(h)); r == 0 {
		return fmt.Errorf("EndDocPrinter(%q): %v", printerName, callErr)
	}
	return nil
}

func mustUTF16(s string) *uint16 {
	p, err := syscall.UTF16PtrFromString(s)
	if err != nil {
		panic(err) // constantes nossas, sem NUL
	}
	return p
}

func discoverPrinters(ctx context.Context) ([]map[string]string, []string) {
	entries, notes := discoverPrintersWindows()
	return entries, notes
}
