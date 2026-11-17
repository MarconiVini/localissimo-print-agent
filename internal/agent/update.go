package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
)

// Auto-update (epic §7; contrato real do Rails: campos agent_* flat no
// payload de config/poll — catálogo printer_agent_releases + alvo por
// negócio). Regras não negociáveis:
//
//   - update NUNCA quebra a impressão: só roda em rodada idle (poll vazio);
//   - binário em download/confirmação = printer-agent.download (0755 já na
//     escrita); sha256 conferido antes de qualquer troca;
//   - pronto = o (version, sha256) da oferta que ORIGINOU o download igual
//     à última oferta recebida — alvo mudou no meio → descarta e re-baixa
//     (nunca instala release retirada/trocada);
//   - spawn do novo confirmado é condição para encerrar; falhou → reverte
//     os renames e segue na versão atual;
//   - a instância nova, com env AGENT_UPDATE_RESTART=1, espera o lock até
//     ~30s; sem lock → exit com erro (nunca duas instâncias);
//   - self-heal no boot: binário sumiu (crash entre os renames) e
//     printer-agent.download existe → renomeia e segue.
const (
	dlFileMode = 0o755
)

type Updater struct {
	mu         sync.Mutex
	dir        string
	ExePath    string // caminho absoluto do executável, capturado no boot
	exeArgs    []string
	OwnVersion string // versão que ESTE binário roda (build -ldflags); injetável
	HTTPSOnly  bool
	client     *Client
	log        *Logger
	dbg        *DebugBuffer
	Spawn      func(exePath string, args []string) error // injetável (testes)

	Offer       *UpdateOffer // última oferta recebida do servidor
	downloaded  *UpdateOffer // oferta cujo binário está em disco, válido
	dlCancel    context.CancelFunc
	dlBusy      bool
	loggedDirty string // motivo já logado para não repetir a cada rodada
}

func NewUpdater(dir, exePath string, exeArgs []string, log *Logger, dbg *DebugBuffer) *Updater {
	return &Updater{
		dir:        dir,
		ExePath:    exePath,
		exeArgs:    exeArgs,
		OwnVersion: Version,
		log:        log,
		dbg:        dbg,
		Spawn:      spawnSelf,
	}
}

// Downloaded devolve a oferta cujo binário válido está em disco (ou nil).
func (u *Updater) Downloaded() *UpdateOffer {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.downloaded
}

// Busy: download em curso?
func (u *Updater) Busy() bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.dlBusy
}

// BindClient liga o client de download (a base define o modo https-only).
func (u *Updater) BindClient(c *Client) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.client = c
	u.HTTPSOnly = c.HTTPSOnly
}

func (u *Updater) DownloadPath() string { return filepath.Join(u.dir, DownloadFileName) }
func (u *Updater) OldPath() string      { return filepath.Join(u.dir, OldFileName) }

// releaseOK: a versão própria casa X.Y(Z) de release? Build sujo de dev
// nunca troca o próprio binário (epic §7.4.1).
func (u *Updater) releaseOK() bool {
	return versionRe.MatchString(u.OwnVersion)
}

// SelfHeal no boot: binário principal sumiu (crash entre os dois renames) e
// o .download existe → renomeia e segue. .old fica de rollback manual.
func (u *Updater) SelfHeal() {
	if _, err := os.Stat(u.ExePath); err == nil {
		return // binário no lugar
	}
	if _, err := os.Stat(u.DownloadPath()); err != nil {
		return // nada para curar
	}
	if err := os.Rename(u.DownloadPath(), u.ExePath); err != nil {
		u.log.Printf("self-heal falhou: renomear %s → %s: %v", DownloadFileName, filepath.Base(u.ExePath), err)
		return
	}
	os.Chmod(u.ExePath, dlFileMode)
	u.log.Printf("self-heal: binário principal sumido, printer-agent.download promovido a executável — seguindo o boot")
}

// CleanupBoot apaga o lixo do swap anterior. Falha (arquivo ainda mapeado,
// típico no Windows) é tolerada e tentada de novo no boot seguinte.
func (u *Updater) CleanupBoot() {
	if _, err := os.Stat(u.OldPath()); err == nil {
		if err := os.Remove(u.OldPath()); err != nil {
			u.log.Printf("não consegui apagar %s (rollback manual continua possível): %v", OldFileName, err)
		}
	}
}

// HandleOffer recebe a última oferta do poll (nil = nenhuma anunciada).
// Roda só em rodada idle — nunca adianta download com comanda na fila.
func (u *Updater) HandleOffer(offer *UpdateOffer) {
	u.mu.Lock()
	defer u.mu.Unlock()

	if offer.equal(u.Offer) {
		// Nada mudou — mas download que falhou antes tem que re-tentar no
		// ciclo seguinte (epic §7.4.2: "falhou → descarta e re-baixa").
		if !u.needsDownloadLocked() {
			return
		}
		u.startDownloadLocked(u.Offer)
		return
	}
	// Alvo mudou (ou sumiu): o download em curso/feito vale para a oferta
	// VELHA — descarta e, se houver oferta nova, re-baixa. EXCETUADO: o
	// arquivo em disco que JÁ confere o sha256 da oferta nova continua
	// válido (o sha é a garantia — não adianta re-baixar o mesmo binário).
	if offer == nil {
		u.discardDownloadLocked()
	} else if sha, err := fileSHA(u.DownloadPath()); err != nil || sha != offer.SHA256 {
		u.discardDownloadLocked()
	}
	u.Offer = offer

	if offer == nil {
		return // sem alvo: fica onde está
	}
	if offer.Version == u.OwnVersion {
		u.dbg.Addf("update: versão alvo %s == rodando — nada a fazer", offer.Version)
		return
	}
	if !u.releaseOK() {
		if u.loggedDirty != "dirty" {
			u.loggedDirty = "dirty"
			u.dbg.Addf("update ignorado: build de dev (%s) não casa versão de release — nunca auto-troca binário de dev", u.OwnVersion)
			u.log.Printf("auto-update ignorado: versão do build (%s) não é release X.Y.Z", u.OwnVersion)
		}
		return
	}
	u.startDownloadLocked(offer)
}

func (u *Updater) startDownloadLocked(offer *UpdateOffer) {
	if u.dlBusy {
		return
	}
	// .download já existe com o sha256 esperado → pula o download (epic
	// §7.4.2 — reuso do arquivo da tentativa anterior/restart).
	if sha, err := fileSHA(u.DownloadPath()); err == nil && sha == offer.SHA256 {
		u.downloaded = offer
		u.dbg.Addf("update: printer-agent.download já em disco com o sha da oferta %s — sem re-baixar", offer.Version)
		return
	}
	u.dlBusy = true
	u.log.Printf("update: versão alvo %s (rodando %s) — iniciando download", offer.Version, u.OwnVersion)
	ctx, cancel := context.WithCancel(context.Background())
	u.dlCancel = cancel
	go func() {
		sha, err := u.client.DownloadToFile(ctx, offer.URL, u.DownloadPath())
		u.mu.Lock()
		u.dlBusy = false
		u.dlCancel = nil
		defer u.mu.Unlock()
		if ctx.Err() != nil {
			// Cancelado: oferta mudou no meio — o arquivo parcial sai e o
			// download da oferta nova começa AGORA (o slot acabou de liberar;
			// sem isso, o re-download ficaria esperando a próxima mudança de
			// oferta que nunca vem).
			os.Remove(u.DownloadPath())
			if u.needsDownloadLocked() {
				u.startDownloadLocked(u.Offer)
			}
			return
		}
		if err != nil {
			u.log.Printf("update: download de %s falhou (segue na versão atual; re-baixa na próxima rodada idle): %v", offer.Version, err)
			u.dbg.Addf("update: download de %s falhou: %v", offer.Version, err)
			os.Remove(u.DownloadPath())
			return
		}
		if !offer.equal(u.Offer) {
			// Trocou de novo enquanto baixava — descarta silenciosamente.
			os.Remove(u.DownloadPath())
			if u.needsDownloadLocked() {
				u.startDownloadLocked(u.Offer)
			}
			return
		}
		if sha != offer.SHA256 {
			// Sha não bate → descarta, loga, segue na atual (epic §7.4.3).
			os.Remove(u.DownloadPath())
			u.log.Printf("update: sha256 do download não bate (esperado %s, veio %s) — descartado, segue na versão atual", offer.SHA256[:12], sha[:12])
			u.dbg.Addf("update: sha256 divergente em %s — descartado", offer.Version)
			return
		}
		u.downloaded = offer
		u.dbg.Addf("update: download de %s pronto (%d bytes... sha confere) — instala na próxima rodada idle", offer.Version, fileSizeOr(u.DownloadPath()))
		u.log.Printf("update: %s baixado e conferido — pronto para o swap", offer.Version)
	}()
}

// needsDownloadLocked: há oferta pendente que ainda não está em disco?
func (u *Updater) needsDownloadLocked() bool {
	if u.Offer == nil || !u.releaseOK() || u.Offer.Version == u.OwnVersion {
		return false
	}
	if u.downloaded != nil && u.downloaded.equal(u.Offer) {
		return false
	}
	if sha, err := fileSHA(u.DownloadPath()); err == nil && sha == u.Offer.SHA256 {
		u.downloaded = u.Offer // arquivo válido já em disco
		return false
	}
	return true
}

func (u *Updater) discardDownloadLocked() {
	if u.dlCancel != nil {
		u.dlCancel()
		u.dlCancel = nil
	}
	u.downloaded = nil
	if u.dlBusy {
		// o goroutine de download limpa o arquivo ao terminar (ctx cancelado)
		return
	}
	os.Remove(u.DownloadPath())
}

// InstallIfReady troca o binário e faz o spawn, se tudo casar. Retorna
// spawned=true quando a instância nova foi criada — o chamador encerra.
func (u *Updater) InstallIfReady(idle bool) bool {
	u.mu.Lock()
	defer u.mu.Unlock()

	if !idle {
		return false
	}
	if u.Offer == nil || u.downloaded == nil || !u.Offer.equal(u.downloaded) {
		return false // sem oferta, sem download pronto ou alvo mudou no meio
	}
	if u.Offer.Version == u.OwnVersion || !u.releaseOK() {
		return false
	}

	u.dbg.Addf("update: swap de %s (rodando %s): rename → .old, promote .download, spawn", u.Offer.Version, u.OwnVersion)
	OldPath := u.OldPath()
	os.Remove(OldPath) // lixo de tentativa anterior (tolerado)

	if err := os.Rename(u.ExePath, OldPath); err != nil {
		u.dbg.Addf("update: rename do binário em execução falhou: %v — segue na atual", err)
		u.log.Printf("update: swap abortado (rename 1): %v", err)
		return false
	}
	if err := os.Rename(u.DownloadPath(), u.ExePath); err != nil {
		// Reverte o primeiro rename — nada mudou para o usuário.
		os.Rename(OldPath, u.ExePath)
		u.dbg.Addf("update: promote do download falhou: %v — renames revertidos", err)
		u.log.Printf("update: swap abortado (rename 2): %v", err)
		return false
	}
	os.Chmod(u.ExePath, dlFileMode) // já é 0755 da escrita; renomear preserva, mas garantir custa nada

	if err := u.Spawn(u.ExePath, u.exeArgs); err != nil {
		// Spawn falho (AV removeu, disco, permissão) → reverte os renames:
		// o novo volta a .download e o .old volta a binário (epic §7.4.4).
		os.Rename(u.ExePath, u.DownloadPath())
		os.Rename(OldPath, u.ExePath)
		u.dbg.Addf("update: spawn falhou (%v) — swap revertido, segue na versão atual", err)
		u.log.Printf("update: spawn falhou, swap revertido: %v", err)
		return false
	}

	u.downloaded = nil
	u.dbg.Addf("update: spawn confirmado da versão %s — encerrando processo antigo", u.Offer.Version)
	u.log.Printf("update: instância nova (%s) em execução — encerrando", u.Offer.Version)
	return true
}

// spawnSelf é o spawn real: mesmo executável (caminho absoluto capturado no
// boot), mesmos argumentos, env com AGENT_UPDATE_RESTART=1.
func spawnSelf(exePath string, args []string) error {
	cmd := exec.Command(exePath, args...)
	cmd.Env = append(os.Environ(), UpdateRestartEnv+"=1")
	cmd.Stdin, cmd.Stdout, cmd.Stderr = nil, nil, nil
	setSpawnAttrs(cmd)
	return cmd.Start()
}

// ---------- utilidades ----------

func writeFileHashing(dstPath string, r io.Reader) (string, error) {
	f, err := os.OpenFile(dstPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, dlFileMode)
	if err != nil {
		return "", fmt.Errorf("criar %s: %w", dstPath, err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(f, h), r); err != nil {
		return "", fmt.Errorf("escrever %s: %w", dstPath, err)
	}
	if err := f.Sync(); err != nil {
		return "", fmt.Errorf("sync %s: %w", dstPath, err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func fileSizeOr(path string) int64 {
	st, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return st.Size()
}

// fileSHA calcula o sha256 de um arquivo em disco.
func fileSHA(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
