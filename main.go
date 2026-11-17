// printer-agent — o cano burro do épico Comandas Térmicas 80mm (S5).
// Rails renderiza (payload ESC/POS pronto em base64); o agente polla, escreve
// na impressora e confirma. Identidade no token.txt ao lado do binário;
// config de impressora vive no servidor (decisão 15).
//
// Este arquivo é o bootstrap; toda a lógica vive em internal/agent
// (testada pelo pacote externo em tests/).
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"runtime"
	"syscall"
	"time"

	"localissimo-print-agent/internal/agent"
)

// exeArgsForRestart: os argumentos originais, repassados ao spawn pós-update
// (mesmos flags — o novo lê o mesmo -server se houver).
func exeArgsForRestart() []string {
	if len(os.Args) <= 1 {
		return nil
	}
	return os.Args[1:]
}

func hostnameOf() string {
	h, err := os.Hostname()
	if err != nil {
		return "?"
	}
	return h
}

func describeToken(rc *agent.RuntimeConfig) string {
	t := agent.ReadToken(rc.TokenPath)
	if t == "" {
		return "ausente (aguardando token.txt)"
	}
	if len(t) <= 8 {
		return "***"
	}
	return t[:4] + "…***"
}

func main() {
	showVersion := flag.Bool("version", false, "imprime a versão e sai")
	serverFlag := flag.String("server", "", "URL base do servidor (dev/tester aponta pro localhost)")
	flag.Parse()

	if *showVersion {
		fmt.Println(agent.Version)
		return
	}

	exePath, err := agent.ResolveExePath()
	if err != nil {
		fmt.Fprintln(os.Stderr, "não consegui resolver o caminho do executável:", err)
		os.Exit(1)
	}
	rc, err := agent.LoadRuntime(exePath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "caminhos de runtime inválidos:", err)
		os.Exit(1)
	}

	log, err := agent.OpenLogger(rc.LogPath, true)
	if err != nil {
		fmt.Fprintln(os.Stderr, "sem agent.log — sigo com stderr apenas:", err)
		log = agent.NewConsoleLogger()
	}
	defer log.Close()

	dbg := agent.NewDebugBuffer()
	// Log local guarda TUDO (linhas de evento + espelho do buffer de debug,
	// prefixo "·") — a diagnose não depende do debug_mode estar ligado. O
	// envio ao servidor é outro canal: só com debug_mode=true na config.
	dbg.Sink = func(msg string) { log.Printf("· %s", msg) }
	log.Printf("printer-agent %s — %s/%s · hostname %s", agent.Version, runtime.GOOS, runtime.GOARCH, hostnameOf())

	upd := agent.NewUpdater(rc.Dir, rc.ExePath, exeArgsForRestart(), log, dbg)

	// Boot §7.4: self-heal antes de tudo (binário sumido + .download → cura),
	// depois o lixo do swap anterior.
	upd.SelfHeal()
	upd.CleanupBoot()

	// Instância única (epic §8). Pós-update: retry ~30s (o processo velho
	// pode ainda estar vivo); sem lock no fim do prazo → exit com erro claro
	// (dois agentes = todo job sai em dobro).
	restarting := os.Getenv(agent.UpdateRestartEnv) == "1"
	var lock *agent.InstanceLock
	if restarting {
		lock, err = agent.RetryLock(rc.LockPath, agent.LockRetryWindow, time.Sleep)
	} else {
		var ok bool
		var lerr error
		lock, ok, lerr = agent.AcquireInstanceLock(rc.LockPath)
		if lerr != nil {
			err = lerr
		} else if !ok {
			err = fmt.Errorf("outro agente está rodando nesta pasta")
		}
	}
	if err != nil {
		log.Printf("instância única: %v — encerrando (nunca duas instâncias; quem religa é o autostart/supervisor)", err)
		os.Exit(1)
	}
	defer lock.Release()
	if restarting {
		log.Printf("lock adquirido após update — instância velha saiu; boot normal da versão %s", agent.Version)
	}

	rc.Server = rc.ServerURL(*serverFlag, log.Printf)
	client := agent.NewClient(rc.Server, func() string {
		return agent.ReadToken(rc.TokenPath)
	})
	upd.BindClient(client) // updater usa o client de verdade para baixar binários

	ag := agent.NewAgent(rc, client, log, dbg, upd)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	log.Printf("boot: servidor %s · token %s", rc.Server, describeToken(rc))
	ag.Run(ctx)
	log.Printf("agente encerrado")
}
