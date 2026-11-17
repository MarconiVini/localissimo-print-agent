//go:build windows

package agent

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"syscall"
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
type printerInfo2W struct {
	Flags              uint32
	_                  uint32
	PrinterName        uintptr
	PortName           uintptr
	DriverName         uintptr
	ShareName          uintptr
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
	if unsafe.Sizeof(printerInfo2W{}) != 128 {
		return fmt.Errorf("layout printerInfo2W inesperado: %d", unsafe.Sizeof(printerInfo2W{}))
	}
	if unsafe.Sizeof(portInfo2W{}) != 32 {
		return fmt.Errorf("layout portInfo2W inesperado: %d", unsafe.Sizeof(portInfo2W{}))
	}
	if unsafe.Sizeof(docInfo1W{}) != 24 {
		return fmt.Errorf("layout docInfo1W inesperado: %d", unsafe.Sizeof(docInfo1W{}))
	}
	return nil
}

// utf16Field lê a string UTF-16 (NUL-terminada) no offset do buffer.
func utf16Field(buf []byte, off uintptr) string {
	if off == 0 || int(off) >= len(buf) {
		return ""
	}
	u16 := unsafe.Slice((*uint16)(unsafe.Pointer(&buf[off])), (len(buf)-int(off))/2)
	return syscall.UTF16ToString(u16)
}

func enumCall(proc *syscall.LazyProc, arg1 uintptr, level uint32) ([]byte, uint32, error) {
	var needed, returned uint32
	// 1ª chamada com buffer nulo só para descobrir o tamanho
	// (retorna FALSE com ERROR_INSUFFICIENT_BUFFER quando há dados).
	_, _, _ = proc.Call(arg1, 0, uintptr(level), 0, 0,
		uintptr(unsafe.Pointer(&needed)), uintptr(unsafe.Pointer(&returned)))
	if needed == 0 {
		return nil, 0, nil
	}
	buf := make([]byte, needed)
	r, _, callErr := proc.Call(arg1, 0, uintptr(level),
		uintptr(unsafe.Pointer(&buf[0])), uintptr(needed),
		uintptr(unsafe.Pointer(&needed)), uintptr(unsafe.Pointer(&returned)))
	if r == 0 {
		return nil, 0, fmt.Errorf("%s: %v", proc.Name, callErr)
	}
	return buf, returned, nil
}

func discoverPrintersWindows() ([]map[string]string, error) {
	if err := checkWinLayouts(); err != nil {
		return nil, err
	}
	pbuf, n, err := enumCall(procEnumPrintersW, printerEnumLocal|printerEnumConnections, 2)
	if err != nil {
		return nil, err
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

	ports := []WinPortRow{}
	if obuf, on, oerr := enumCall(procEnumPortsW, 0, 2); oerr == nil {
		for i := uint32(0); i < on; i++ {
			po := (*portInfo2W)(unsafe.Pointer(&obuf[uintptr(i)*unsafe.Sizeof(portInfo2W{})]))
			ports = append(ports, WinPortRow{
				Name:        utf16Field(obuf, po.PortName),
				Description: utf16Field(obuf, po.Description),
			})
		}
		// EnumPorts falho não derruba o report — só perde Host/Porta.
	}

	lookup := TCPLookupFromPorts(ports, lookupTCPPort)
	return WindowsEntries(rows, ports, lookup), nil
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
func writeUSBPlatform(ctx context.Context, printerName string, payload []byte) error {
	return writeUSBWindows(printerName, payload)
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

	doc := docInfo1W{
		DocName:    uintptr(unsafe.Pointer(mustUTF16("Localissimo comanda"))),
		OutputFile: 0,
		Datatype:   uintptr(unsafe.Pointer(mustUTF16("RAW"))), // DataType RAW — passa direto pela fila
	}
	if r, _, callErr := procStartDocPrinterW.Call(uintptr(h), 1, uintptr(unsafe.Pointer(&doc))); r == 0 || r == ^uintptr(0) {
		return fmt.Errorf("StartDocPrinter(%q): %v", printerName, callErr)
	}
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

func discoverPrinters(ctx context.Context) []map[string]string {
	entries, err := discoverPrintersWindows()
	if err != nil {
		// Falha de coleta não trava o boot: report vazio, motivo no log.
		return nil
	}
	return entries
}
