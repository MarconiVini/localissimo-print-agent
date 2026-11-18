package tests

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"localissimo-print-agent/internal/agent"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func hexOf(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// Updater de teste: exe fake na temp dir, spawn gravável, client no fake
// servidor de binários (download via httptest).
func newTestUpdater(t *testing.T, dir string) (*agent.Updater, *[]string, *httptest.Server, func(path string, body []byte) string) {
	t.Helper()
	exe := filepath.Join(dir, "printer-agent")
	if err := os.WriteFile(exe, []byte("EXE-VELHO"), 0o755); err != nil {
		t.Fatal(err)
	}
	var spawned []string
	var mu sync.Mutex
	log := agent.NewNullLogger()
	upd := agent.NewUpdater(dir, exe, []string{"-server", "http://localhost"}, log, agent.NewDebugBuffer())
	upd.OwnVersion = "0.1.0" // release limpa (testes não são build sujo)
	upd.Spawn = func(path string, args []string) error {
		mu.Lock()
		spawned = append(spawned, path+" "+strings.Join(args, " "))
		mu.Unlock()
		return nil
	}

	binaries := map[string][]byte{}
	mu2 := sync.Mutex{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu2.Lock()
		body, ok := binaries[r.URL.Path]
		mu2.Unlock()
		if !ok {
			w.WriteHeader(404)
			return
		}
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	upd.BindClient(agent.NewClient(srv.URL, func() string { return "tok" }))

	serve := func(path string, body []byte) string {
		mu2.Lock()
		binaries[path] = body
		mu2.Unlock()
		return srv.URL + path
	}
	return upd, &spawned, srv, serve
}

func offerFor(url, version, sha string) *agent.UpdateOffer {
	return &agent.UpdateOffer{Version: version, URL: url, SHA256: sha}
}

// Build sujo (dev) ignora o update com motivo no log — nunca troca binário
// de dev (epic §7.4.1).
func TestUpdateBuildSujoIgnora(t *testing.T) {
	dir := t.TempDir()
	upd, spawned, _, serve := newTestUpdater(t, dir)
	upd.OwnVersion = "0.0.0-dev"
	url := serve("/bin", []byte("novo"))
	upd.HandleOffer(offerFor(url, "9.9.9", hexOf([]byte("novo"))))
	time.Sleep(80 * time.Millisecond) // se (por bug) baixasse, daria tempo
	if len(*spawned) != 0 {
		t.Fatalf("build sujo nunca instala")
	}
	if _, err := os.Stat(upd.DownloadPath()); err == nil {
		t.Fatalf("build sujo não deveria nem baixar")
	}
	busy := upd.Busy()
	if busy {
		t.Fatalf("build sujo não dispara download")
	}
}

// Fluxo completo feliz: oferta → download (0755) → sha confere → na rodada
// idle seguinte com oferta INALTERADA → swap (exe↔.old/.download) + spawn →
// exit do processo velho.
func TestUpdateFluxoFeliz(t *testing.T) {
	dir := t.TempDir()
	upd, spawned, _, serve := newTestUpdater(t, dir)
	novo := []byte("EXE-NOVO")
	url := serve("/printer-agent-linux-amd64", novo)
	offer := offerFor(url, "0.2.0", hexOf(novo))

	// Rodada idle com oferta: dispara o download (concorrente ao poll).
	upd.HandleOffer(offer)
	waitFor(t, 2*time.Second, func() bool {
		return upd.Downloaded() != nil
	}, "download ficar pronto")
	if _, err := os.Stat(upd.DownloadPath()); err != nil {
		t.Fatalf("printer-agent.download deveria existir: %v", err)
	}

	// Instalação: idle=true + oferta casando com o download → swap+spawn.
	if !upd.InstallIfReady() {
		t.Fatalf("InstallIfReady deveria instalar")
	}
	if len(*spawned) != 1 {
		t.Fatalf("spawn não confirmado: %v", *spawned)
	}
	if got, _ := os.ReadFile(upd.ExePath); string(got) != "EXE-NOVO" {
		t.Fatalf("binário novo deveria estar no caminho do exe: %q", got)
	}
	if got, _ := os.ReadFile(upd.OldPath()); string(got) != "EXE-VELHO" {
		t.Fatalf("binário velho deveria estar no .old (rollback manual): %q", got)
	}
	if _, err := os.Stat(upd.DownloadPath()); !os.IsNotExist(err) {
		t.Fatalf("download deveria ter sido promovido (sumir da pasta): err=%v", err)
	}

	// Idempotente: rodada seguinte (nova instância, versão igual) não re-instala.
	upd.OwnVersion = "0.2.0"
	upd.HandleOffer(offer)
	if upd.InstallIfReady() {
		t.Fatalf("versão igual à oferta não re-instala")
	}
}

// Modo serviço (-spawnless): a troca NÃO cria filho — renames idênticos
// (binário novo no ExePath, velho no .old) e InstallIfReady devolve true
// para o processo encerrar com falha; quem religa é o supervisor
// (SCM/systemd — exit ≠ 0 é o contrato, cf. main.go: errUpdateExit).
func TestUpdateSpawnlessNaoSpawna(t *testing.T) {
	dir := t.TempDir()
	upd, spawned, _, serve := newTestUpdater(t, dir)
	upd.Spawnless = true
	novo := []byte("EXE-NOVO")
	url := serve("/printer-agent-linux-amd64", novo)
	offer := offerFor(url, "0.2.0", hexOf(novo))

	upd.HandleOffer(offer)
	waitFor(t, 2*time.Second, func() bool {
		return upd.Downloaded() != nil
	}, "download ficar pronto")

	if !upd.InstallIfReady() {
		t.Fatalf("InstallIfReady deveria instalar mesmo em modo serviço")
	}
	if len(*spawned) != 0 {
		t.Fatalf("modo serviço não pode spawnar filho: %v", *spawned)
	}
	if !upd.Swapped() {
		t.Fatalf("Swapped deveria sinalizar a troca concluída")
	}
	if got, _ := os.ReadFile(upd.ExePath); string(got) != "EXE-NOVO" {
		t.Fatalf("binário novo deveria estar no caminho do exe: %q", got)
	}
	if got, _ := os.ReadFile(upd.OldPath()); string(got) != "EXE-VELHO" {
		t.Fatalf("binário velho deveria estar no .old (rollback manual): %q", got)
	}
}

// Update só em rodada idle — nunca no meio de um lote (epic §7.3). A regra
// vive no Round (fonte única, sem parâmetro no InstallIfReady): rodada com
// jobs nem passa a oferta ao updater; a vazia seguinte entrega.
func TestUpdateSoEmRodadaIdle(t *testing.T) {
	dir := t.TempDir()
	writeTokenFile(t, dir, "tok")
	rails := newFakeRails("tok")
	srv := startRails(t, rails)
	a := newTestAgent(t, dir, srv.URL, rails)
	fp := startFakePrinter(t)
	setRailsCfg(rails, fp, map[string]any{
		"agent_version":      "0.2.0",
		"agent_download_url": srv.URL + "/nao-existe.bin", // download falha — irrelevante aqui
		"agent_sha256":       strings.Repeat("0", 64),
	})
	rails.setJobs([]map[string]any{jobJSON(901, "kitchen", payloadComanda("u"))})

	if r := a.Round(context.Background()); r.Exit {
		t.Fatalf("rodada com jobs não pode pedir exit")
	}
	if a.Updater.Offer != nil {
		t.Fatalf("rodada com lote nem pode ver a oferta (janela fechada): %+v", a.Updater.Offer)
	}

	rails.setJobs(nil)
	a.Round(context.Background()) // idle → janela abre: a oferta chega ao updater
	if a.Updater.Offer == nil || a.Updater.Offer.Version != "0.2.0" {
		t.Fatalf("rodada idle deve entregar a oferta ao updater: %+v", a.Updater.Offer)
	}
	if a.Updater.InstallIfReady() {
		t.Fatalf("sem download pronto não instala")
	}
}

// Sha divergente → descarta e segue na versão atual (epic §7.4.3).
func TestUpdateShaDivergenteDescarta(t *testing.T) {
	dir := t.TempDir()
	upd, spawned, _, serve := newTestUpdater(t, dir)
	url := serve("/bin", []byte("conteudo-malicioso-ou-corrompido"))
	offer := offerFor(url, "0.2.0", strings.Repeat("ab", 32)) // sha que NÃO bate
	upd.HandleOffer(offer)
	waitFor(t, 2*time.Second, func() bool {
		return upd.Downloaded() != nil || !upd.Busy()
	}, "download terminar (com sha errado)")
	ready := upd.Downloaded() != nil
	if ready {
		t.Fatalf("sha divergente não pode marcar como pronto")
	}
	if _, err := os.Stat(upd.DownloadPath()); !os.IsNotExist(err) {
		t.Fatalf("download com sha errado deveria ser descartado: err=%v", err)
	}
	if upd.InstallIfReady() || len(*spawned) != 0 {
		t.Fatalf("nada instalado com sha divergente")
	}
}

// Alvo mudou no meio (withdraw/troca) → descarta o download velho e
// re-baixa; nunca instala release retirada (epic §7.4.4).
func TestUpdateOfertaMudouDescartaDownload(t *testing.T) {
	dir := t.TempDir()
	upd, spawned, _, serve := newTestUpdater(t, dir)
	a := []byte("BINARIO-A")
	urlA := serve("/a", a)
	offerA := offerFor(urlA, "0.2.0", hexOf(a))
	upd.HandleOffer(offerA)
	waitFor(t, 2*time.Second, func() bool {
		return upd.Downloaded() != nil
	}, "download A ficar pronto")

	// Servidor troca o alvo para B (outro sha): o .download de A vale zero.
	b := []byte("BINARIO-B")
	urlB := serve("/b", b)
	offerB := offerFor(urlB, "0.3.0", hexOf(b))
	upd.HandleOffer(offerB)
	waitFor(t, 2*time.Second, func() bool {
		return upd.Downloaded() != nil && upd.Downloaded().Version == "0.3.0"
	}, "download B ficar pronto")

	// A oferta velha não instala mais (download dela foi descartado).
	upd.Offer = offerA // força a comparação
	if upd.InstallIfReady() {
		t.Fatalf("release velha não pode instalar após o alvo mudar")
	}
	if len(*spawned) != 0 {
		t.Fatalf("spawn da release velha!")
	}

	// E a oferta vigente B instala (após re-baixar, o que é assíncrono).
	upd.HandleOffer(offerB)
	waitFor(t, 2*time.Second, func() bool {
		return upd.Downloaded() != nil && upd.Downloaded().Version == "0.3.0"
	}, "re-download de B ficar pronto")
	if !upd.InstallIfReady() {
		t.Fatalf("oferta vigente deveria instalar")
	}
}

// Alvo sumiu (withdraw no Rails: alvo removido do negócio) → nada instalado
// e o download pendente é limpo.
func TestUpdateOfertaSumiu(t *testing.T) {
	dir := t.TempDir()
	upd, spawned, _, serve := newTestUpdater(t, dir)
	novo := []byte("EXE")
	url := serve("/bin", novo)
	upd.HandleOffer(offerFor(url, "0.2.0", hexOf(novo)))
	waitFor(t, 2*time.Second, func() bool {
		return upd.Downloaded() != nil
	}, "download ficar pronto")

	upd.HandleOffer(nil) // sem alvo: fica onde está
	waitFor(t, 2*time.Second, func() bool {
		_, err := os.Stat(upd.DownloadPath())
		return os.IsNotExist(err)
	}, "download pendente ser limpo")
	if upd.InstallIfReady() || len(*spawned) != 0 {
		t.Fatalf("sem oferta não instala nada")
	}
}

// Spawn falhou (AV removeu o binário, disco, permissão) → reverte os renames
// e segue na versão atual (epic §7.4.4).
func TestUpdateSpawnFalhoReverte(t *testing.T) {
	dir := t.TempDir()
	upd, _, _, serve := newTestUpdater(t, dir)
	novo := []byte("EXE-NOVO")
	url := serve("/bin", novo)
	upd.HandleOffer(offerFor(url, "0.2.0", hexOf(novo)))
	waitFor(t, 2*time.Second, func() bool {
		return upd.Downloaded() != nil
	}, "download ficar pronto")

	upd.Spawn = func(path string, args []string) error {
		return context.DeadlineExceeded // CreateProcess falhou
	}
	if upd.InstallIfReady() {
		t.Fatalf("spawn falho não confirma update")
	}
	// Estado revertido: exe = velho, .download = novo, .old não existe.
	if got, _ := os.ReadFile(upd.ExePath); string(got) != "EXE-VELHO" {
		t.Fatalf("exe deveria ter voltado ao binário velho: %q", got)
	}
	if got, _ := os.ReadFile(upd.DownloadPath()); string(got) != "EXE-NOVO" {
		t.Fatalf("novo deveria voltar a .download: %q", got)
	}
	if _, err := os.Stat(upd.OldPath()); !os.IsNotExist(err) {
		t.Fatalf(".old deveria ter sido consumido pelo revert: err=%v", err)
	}
	// E a próxima rodada pode tentar de novo (spawn consertado).
	upd.Spawn = func(path string, args []string) error { return nil }
	if !upd.InstallIfReady() {
		t.Fatalf("nova tentativa na rodada seguinte deveria instalar")
	}
}

// Download existente com o sha esperado → pula o download (epic §7.4.2).
func TestUpdatePulaDownloadComShaNovoNaPasta(t *testing.T) {
	dir := t.TempDir()
	upd, spawned, srv, serve := newTestUpdater(t, dir)
	_ = srv
	novo := []byte("EXE-NOVO")
	url := serve("/bin", novo)
	offer := offerFor(url, "0.2.0", hexOf(novo))

	// Pré-cria o .download já com o conteúdo/sha esperado.
	if err := os.WriteFile(upd.DownloadPath(), novo, 0o755); err != nil {
		t.Fatal(err)
	}

	// Derruba o servidor de download: sem rede, o .download válido basta.
	srv.Close()
	upd.HandleOffer(offer)
	waitFor(t, 2*time.Second, func() bool {
		return upd.Downloaded() != nil
	}, "revalidação do arquivo existente")
	if !upd.InstallIfReady() || len(*spawned) != 1 {
		t.Fatalf("download pré-existente válido deveria instalar sem rede")
	}
}

// .old de tentativa anterior não bloqueia o próximo swap.
func TestUpdateLimpaOldAnterior(t *testing.T) {
	dir := t.TempDir()
	upd, _, _, serve := newTestUpdater(t, dir)
	os.WriteFile(upd.OldPath(), []byte("lixo-do-swap-passado"), 0o755)

	novo := []byte("EXE-NOVO")
	url := serve("/bin", novo)
	upd.HandleOffer(offerFor(url, "0.2.0", hexOf(novo)))
	waitFor(t, 2*time.Second, func() bool {
		return upd.Downloaded() != nil
	}, "download ficar pronto")

	if !upd.InstallIfReady() {
		t.Fatalf("swap deveria limpar o .old anterior e prosseguir")
	}
}

// Self-heal no boot (epic §7.4): binário principal sumiu (crash entre os
// renames) e .download existe → renomeia e segue.
func TestSelfHeal(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "printer-agent")
	// Cenário do crash: exe não existe, .download é o binário novo.
	if err := os.WriteFile(filepath.Join(dir, agent.DownloadFileName), []byte("EXE-NOVO"), 0o755); err != nil {
		t.Fatal(err)
	}
	log := agent.NewNullLogger()
	upd := agent.NewUpdater(dir, exe, nil, log, agent.NewDebugBuffer())
	upd.SelfHeal()
	if got, err := os.ReadFile(exe); err != nil || string(got) != "EXE-NOVO" {
		t.Fatalf("self-heal deveria promover o .download: %v %q", err, got)
	}
	st, _ := os.Stat(exe)
	if st.Mode().Perm() != 0o755 {
		t.Fatalf("exe curado deve ser executável: %v", st.Mode().Perm())
	}
	if _, err := os.Stat(filepath.Join(dir, agent.DownloadFileName)); !os.IsNotExist(err) {
		t.Fatalf(".download deveria ter sido consumido")
	}

	// Sem .download e sem exe: nada a fazer, sem crash.
	dir2 := t.TempDir()
	upd2 := agent.NewUpdater(dir2, filepath.Join(dir2, "printer-agent"), nil, log, agent.NewDebugBuffer())
	upd2.SelfHeal() // não pode panicar
}

// CleanupBoot apaga o lixo do swap (tolerando falha — arquivo mapeado).
func TestCleanupBoot(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "printer-agent")
	log := agent.NewNullLogger()
	upd := agent.NewUpdater(dir, exe, nil, log, agent.NewDebugBuffer())

	os.WriteFile(upd.OldPath(), []byte("velho"), 0o755)
	upd.CleanupBoot()
	if _, err := os.Stat(upd.OldPath()); !os.IsNotExist(err) {
		t.Fatalf(".old deveria ser apagado no boot: %v", err)
	}

	// .old travado (diretório no lugar — apagar falha) é tolerado.
	os.Mkdir(upd.OldPath(), 0o755)
	upd.CleanupBoot() // não pode panicar; avisa no log
	os.Remove(upd.OldPath())
}

// Troca de alvo NO MEIO do download (withdraw/troca no Rails): A cancelado →
// B começa sem depender de nova mudança de oferta — e B instala.
func TestUpdateTrocaNoMeioDoDownload(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "printer-agent")
	os.WriteFile(exe, []byte("EXE-VELHO"), 0o755)
	log := agent.NewNullLogger()
	upd := agent.NewUpdater(dir, exe, nil, log, agent.NewDebugBuffer())
	upd.OwnVersion = "0.1.0"

	binB := []byte("BINARIO-B")
	release := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/a-lenta" {
			time.Sleep(400 * time.Millisecond) // download A lento
			_, _ = w.Write([]byte("BINARIO-A"))
			return
		}
		_, _ = w.Write(binB)
	}))
	defer release.Close()
	upd.BindClient(agent.NewClient(release.URL, func() string { return "tok" }))

	offerA := offerFor(release.URL+"/a-lenta", "0.2.0", hexOf([]byte("BINARIO-A")))
	upd.HandleOffer(offerA)

	// O alvo muda enquanto A baixa: B tem que baixar e instalar por conta
	// própria (sem nova mudança de oferta).
	offerB := offerFor(release.URL+"/b", "0.3.0", hexOf(binB))
	upd.HandleOffer(offerB)

	waitFor(t, 3*time.Second, func() bool {
		return upd.Downloaded() != nil && upd.Downloaded().Version == "0.3.0"
	}, "download de B começar e terminar após cancelamento de A")

	upd.Spawn = func(path string, args []string) error { return nil }
	if !upd.InstallIfReady() {
		t.Fatalf("B deveria instalar após o cancelamento de A")
	}
	if got, _ := os.ReadFile(exe); string(got) != "BINARIO-B" {
		t.Fatalf("exe deveria conter B: %q", got)
	}
}

// Download que falhou → re-baixa no ciclo seguinte com a MESMA oferta
// (epic §7.4.2: falhou → descarta e re-baixa no próximo ciclo).
func TestUpdateRebaixaAposFalha(t *testing.T) {
	dir := t.TempDir()
	upd, _, srv, serve := newTestUpdater(t, dir)
	novo := []byte("EXE-NOVO")
	url := serve("/bin", novo)
	offer := offerFor(url, "0.2.0", hexOf(novo))

	// Servidor fora do ar na 1ª tentativa: download falha.
	srv.Close()
	upd.HandleOffer(offer)
	waitFor(t, 2*time.Second, func() bool {
		return !upd.Busy()
	}, "download falhar com servidor morto")
	pronto := upd.Downloaded() != nil
	if pronto {
		t.Fatalf("sem servidor não pode marcar pronto")
	}

	// Servidor volta (novo httptest na mesma url é impossível — mas o
	// HandleOffer com a MESMA oferta re-baixa assim que o arquivo em disco
	// não existe; simulamos com um novo servidor no MESMO caminho usando um
	// listener reaberto manualmente).
	ln, err := net.Listen("tcp", strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Skipf("não consegui reabrir a porta do servidor de teste: %v", err)
	}
	mux2 := http.NewServeMux()
	mux2.HandleFunc("/bin", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(novo)
	})
	srv2 := &http.Server{Handler: mux2}
	go srv2.Serve(ln)
	defer srv2.Close()

	upd.HandleOffer(offer) // mesma oferta → re-tenta o download
	waitFor(t, 2*time.Second, func() bool {
		return upd.Downloaded() != nil
	}, "re-download após servidor voltar")
	if !upd.InstallIfReady() {
		t.Fatalf("deveria instalar após re-download")
	}
}
