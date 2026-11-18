// printer-agent — o cano burro do épico Comandas Térmicas 80mm (S5).
// Rails renderiza (payload ESC/POS pronto em base64); o agente polla, escreve
// na impressora e confirma. Identidade no token.txt ao lado do binário;
// config de impressora vive no servidor (decisão 15).
//
// Este arquivo é o bootstrap; toda a lógica vive em internal/agent
// (testada pelo pacote externo em tests/).
//
// Dois modos de execução, mesmo binário:
//   - console (comportamento de sempre): sinais/Ctrl+C, spawn pós-update;
//   - serviço nativo no Windows (internal/winsvc): o SCM sobe no boot sem
//     login e religa em falha — não há watcher; quem vigia é o OS. Os
//     instaladores (packaging/) rodam com -spawnless: o pós-update não
//     spawna filho, apenas encerra com falha e o supervisor religa.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"runtime"
	"syscall"
	"time"

	"localissimo-print-agent/internal/agent"
	"localissimo-print-agent/internal/winsvc"
)

// errUpdateExit: o loop acabou porque o update trocou o binário no lugar.
// No console é saída limpa (exit 0) — a instância nova já foi criada via
// spawn. Sob supervisor (SCM/systemd, -spawnless) é FALHA proposital:
// exit ≠ 0 faz o supervisor religar, agora no binário novo.
var errUpdateExit = errors.New("update instalado — encerrando para o supervisor religar")

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
	serverFlag := flag.String("server", "", "URL base do servidor (dev/tester aponta pro localhost; sem flag nem config.json = produção)")
	debugFlag := flag.Bool("debug", false, "força modo console (Ctrl+C), mesmo quando iniciado pelo gerenciador de serviços")
	spawnlessFlag := flag.Bool("spawnless", false, "não spawna o binário pós-update — encerra e o supervisor religa (instaladores usam)")
	flag.Parse()

	if *showVersion {
		fmt.Println(agent.Version)
		return
	}

	// Serviço nativo do Windows: o SCM é o dono do ciclo de vida (boot sem
	// login, restart com backoff via sc failure, stop gracioso via ctx).
	if winsvc.UnderSCM() && !*debugFlag {
		os.Exit(winsvc.Run(winsvc.ServiceName, func(ctx context.Context) error {
			return bootAndRun(*serverFlag, *spawnlessFlag, ctx)
		}))
	}

	// Console (comportamento de sempre): sinais de teclado/sistema. O
	// errUpdateExit aqui é saída limpa — a instância nova já roda.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := bootAndRun(*serverFlag, *spawnlessFlag, ctx); err != nil && !errors.Is(err, errUpdateExit) {
		os.Exit(1)
	}
}

// bootAndRun é o boot em 4 passos (epic §4) + loop (§5), um "run" por
// processo: o console chama uma vez e traduz o retorno em exit code; o SCM
// chama de novo a cada religada. O ctx vem de fora: sinais no console,
// Stop/Shutdown no serviço. Erro de retorno = falha do processo (no serviço,
// vira exit ≠ 0 → o SCM religa; boot de pasta quebrada então fica visível
// no services.msc em vez de sumir).
func bootAndRun(serverFlag string, spawnless bool, ctx context.Context) error {
	exePath, err := agent.ResolveExePath()
	if err != nil {
		fmt.Fprintln(os.Stderr, "não consegui resolver o caminho do executável:", err)
		return fmt.Errorf("resolver executável: %w", err)
	}
	rc, err := agent.LoadRuntime(exePath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "caminhos de runtime inválidos:", err)
		return fmt.Errorf("runtime: %w", err)
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
	upd.Spawnless = spawnless

	// Boot §7.4: self-heal antes de tudo (binário sumido + .download → cura),
	// depois o lixo do swap anterior.
	upd.SelfHeal()
	upd.CleanupBoot()

	// Instância única (epic §8). Pós-update: retry ~30s (o processo velho
	// pode ainda estar vivo); sem lock no fim do prazo → erro claro (dois
	// agentes = todo job sai em dobro). Sob supervisor o erro de retorno
	// vira religada com backoff — a fila do lock se resolve sozinha.
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
		return fmt.Errorf("instância única: %w", err)
	}
	defer lock.Release()
	if restarting {
		log.Printf("lock adquirido após update — instância velha saiu; boot normal da versão %s", agent.Version)
	}

	rc.Server = rc.ServerURL(serverFlag, log.Printf)
	client := agent.NewClient(rc.Server, func() string {
		return agent.ReadToken(rc.TokenPath)
	})
	upd.BindClient(client) // updater usa o client de verdade para baixar binários

	ag := agent.NewAgent(rc, client, log, dbg, upd)

	log.Printf("boot: servidor %s · token %s", rc.Server, describeToken(rc))
	ag.Run(ctx)
	log.Printf("agente encerrado")
	if upd.Swapped() {
		return errUpdateExit
	}
	return nil
}
