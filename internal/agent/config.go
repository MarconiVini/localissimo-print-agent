package agent

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// Configuração de runtime do agente (epic §3): stateless em config de
// impressora — tudo que o agente precisa no disco é o token e, opcionalmente,
// o servidor. Precedência do servidor (epic §3): default compilado
// < config.json < flag -server.
const (
	DefaultServer    = "https://localissimo.com.br"
	TokenFileName    = "token.txt"
	serverConfigFile = "config.json"
	logFileName      = "agent.log"
	LockFileName     = "agent.lock"
	DownloadFileName = "printer-agent.download"
	OldFileName      = "printer-agent.old"
	UpdateRestartEnv = "AGENT_UPDATE_RESTART"
)

// RuntimeConfig é resolvido uma vez no boot; os caminhos são todos relativos
// à pasta do binário (portável nos 3 OS).
type RuntimeConfig struct {
	Server     string // base URL sem barra final
	Dir        string // pasta do binário
	ExePath    string // caminho absoluto do executável, capturado no boot
	TokenPath  string
	ServerConf string
	LogPath    string
	LockPath   string
}

// ResolveExePath captura o caminho do executável UMA vez no boot — depois do
// rename dance do auto-update, os caminhos derivados dele continuam válidos
// (o novo processo recomeça o boot com a própria captura).
func ResolveExePath() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		// Fallback raríssimo (argv[0] absoluto) — segue o espírito portável.
		if len(os.Args) == 0 {
			return "", fmt.Errorf("os.Executable falhou: %w", err)
		}
		abs, aerr := filepath.Abs(os.Args[0])
		if aerr != nil {
			return "", fmt.Errorf("os.Executable falhou (%v) e argv[0] sem caminho absoluto", err)
		}
		exe = abs
	}
	return filepath.Abs(exe)
}

// LoadRuntime monta a configuração de caminhos + URL do servidor.
func LoadRuntime(exePath string) (*RuntimeConfig, error) {
	if exePath == "" {
		return nil, fmt.Errorf("caminho do executável vazio")
	}
	dir := filepath.Dir(exePath)
	rc := &RuntimeConfig{
		Dir:        dir,
		ExePath:    exePath,
		TokenPath:  filepath.Join(dir, TokenFileName),
		ServerConf: filepath.Join(dir, serverConfigFile),
		LogPath:    filepath.Join(dir, logFileName),
		LockPath:   filepath.Join(dir, LockFileName),
	}
	return rc, nil
}

// ServerURL aplica a precedência flag > config.json > default compilado.
// Um config.json malformado não derruba o agente: avisa e segue na próxima
// da precedência (config de servidor é opcional; brickar o balcão por um
// JSON quebrado seria o oposto do contrato).
func (rc *RuntimeConfig) ServerURL(serverFlag string, warnf func(format string, args ...any)) string {
	if s := NormalizeServer(serverFlag); s != "" {
		return s
	}
	if data, err := os.ReadFile(rc.ServerConf); err == nil {
		var conf struct {
			Server string `json:"server"`
		}
		if err := json.Unmarshal(data, &conf); err != nil {
			if warnf != nil {
				warnf("config.json inválido (%v) — ignorado; usando servidor padrão", err)
			}
		} else if s := NormalizeServer(conf.Server); s != "" {
			return s
		} else if warnf != nil && strings.TrimSpace(conf.Server) != "" {
			warnf("config.json com server inválido (%q) — ignorado; usando servidor padrão", conf.Server)
		}
	}
	return DefaultServer
}

// NormalizeServer trim + barra final; exige http(s):// — dev usa http://,
// produção https:// (epic §8: nunca desligar verificação de TLS; o agente
// não inventa esquema).
func NormalizeServer(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimRight(s, "/")
	if s == "" {
		return ""
	}
	u, err := url.Parse(s)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return ""
	}
	return s
}

// ReadToken lê o token.txt (trim). Ausente/vazio → "" (o chamador decide o
// retry — epic §3 passo 1: o arquivo pode chegar depois; o agente nunca sai).
// Relido a cada tentativa de boot/retry: trocar o token é editar o arquivo,
// sem restart (epic do Rails §3 passo 3).
func ReadToken(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}
