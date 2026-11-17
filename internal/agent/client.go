package agent

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Superfície HTTP (epic §9 — contrato congelado do S4 + acréscimos):
//
//	POST /impressora/printers      boot passo 2
//	GET  /impressora/config        boot passo 3 + loop do configured=false
//	GET  /impressora/jobs          cada poll (heartbeat)
//	POST /impressora/jobs/:id/ack  após cada write ok
//	POST /impressora/logs          cada rodada com debug_mode
const (
	apiTimeout      = 15 * time.Second
	downloadTimeout = 5 * time.Minute
	// Tetos do servidor (não adianta exceder): report 50×20 chaves,
	// logs 200×2000. O corpo do poll é limitado por sanidade de leitura.
	maxResponseBody = 32 << 20
)

// HTTPError distingue resposta de servidor de falha de rede — o agente
// classifica: 404 = token/flag (retry fixo 30s), 5xx/timeout = backoff,
// demais 4xx = log + backoff.
type HTTPError struct {
	Status int
	Body   string
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("http %d: %.300s", e.Status, emUmaLinha(e.Body))
}

// emUmaLinha: corpo de erro (página HTML do Rails, JSON com quebra…) vira
// UMA linha — sem isso a "linha" do agent.log quebra em dezenas de linhas
// sem timestamp (páginas de exceção do Rails têm \r\n no head inteiro).
func emUmaLinha(s string) string {
	return strings.NewReplacer("\r", " ", "\n", " ", "\t", " ").Replace(s)
}

// UpdateOffer é a oferta de auto-update embutida na config do poll
// (campos agent_version / agent_download_url / agent_sha256). Só vale
// quando os três vieram — pela metade não é oferta (epic Rails §7.1:
// sem alvo, os campos vão null e o agente fica na versão atual).
type UpdateOffer struct {
	Version string
	URL     string
	SHA256  string
}

func (o *UpdateOffer) equal(other *UpdateOffer) bool {
	if o == nil || other == nil {
		return o == other
	}
	return o.Version == other.Version && o.URL == other.URL && o.SHA256 == other.SHA256
}

// DeviceConfig é a config do device (mesma hash do lado Rails —
// Business#impressora_config_payload). columns/codepage/auto_cut/name são
// informativos (input do render no Rails) e NEM chegam aqui: o agente é
// cano burro (decisão 8).
type DeviceConfig struct {
	Configured    bool
	ConfigVersion string
	Connection    string
	Host          string
	Port          int
	PrinterName   string
	PollInterval  int // segundos; ≤0/ausente → default 3 no clamp
	DebugMode     bool
	Offer         *UpdateOffer
}

type Job struct {
	ID      int64
	Kind    string // "kitchen" | "delivery" — só para log
	Payload []byte // ESC/POS verbatim (base64 std decodificado)
}

type Client struct {
	base      string // sem barra final
	token     func() string
	API       *http.Client
	DL        *http.Client
	HTTPSOnly bool // base https → download e redirects só https
}

func NewClient(base string, token func() string) *Client {
	return &Client{
		base:      strings.TrimRight(base, "/"),
		token:     token,
		HTTPSOnly: strings.HasPrefix(strings.TrimRight(base, "/"), "https://"),
		API:       &http.Client{Timeout: apiTimeout},
		DL:        &http.Client{Timeout: downloadTimeout},
	}
}

func (c *Client) newRequest(ctx context.Context, method, path string, body []byte) (*http.Request, error) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, reader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token())
	req.Header.Set("X-Agent-Version", Version)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return req, nil
}

func (c *Client) do(ctx context.Context, method, path string, body []byte) ([]byte, error) {
	req, err := c.newRequest(ctx, method, path, body)
	if err != nil {
		return nil, err
	}
	resp, err := c.API.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBody))
	if err != nil {
		return nil, fmt.Errorf("ler resposta %s: %w", path, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, &HTTPError{Status: resp.StatusCode, Body: string(data)}
	}
	return data, nil
}

// ---------- payloads JSON do servidor ----------

type ConfigPayload struct {
	Configured    *bool   `json:"configured"`
	ConfigVersion *string `json:"config_version"`
	Connection    *string `json:"connection"`
	Host          *string `json:"host"`
	Port          *int    `json:"port"`
	PrinterName   *string `json:"printer_name"`
	PollInterval  *int    `json:"poll_interval"`
	DebugMode     *bool   `json:"debug_mode"`
	AgentVersion  *string `json:"agent_version"`
	AgentURL      *string `json:"agent_download_url"`
	AgentSHA      *string `json:"agent_sha256"`
}

func (j *ConfigPayload) ToConfig() *DeviceConfig {
	cfg := &DeviceConfig{}
	if j.Configured != nil {
		cfg.Configured = *j.Configured
	}
	if j.ConfigVersion != nil {
		cfg.ConfigVersion = *j.ConfigVersion
	}
	if j.Connection != nil {
		cfg.Connection = strings.ToLower(strings.TrimSpace(*j.Connection))
	}
	if j.Host != nil {
		cfg.Host = strings.TrimSpace(*j.Host)
	}
	if j.Port != nil {
		cfg.Port = *j.Port
	}
	if j.PrinterName != nil {
		cfg.PrinterName = strings.TrimSpace(*j.PrinterName)
	}
	if j.PollInterval != nil {
		cfg.PollInterval = *j.PollInterval
	}
	if j.DebugMode != nil {
		cfg.DebugMode = *j.DebugMode
	}
	if av, au, as := j.AgentVersion, j.AgentURL, j.AgentSHA; av != nil && au != nil && as != nil &&
		strings.TrimSpace(*av) != "" && strings.TrimSpace(*au) != "" && strings.TrimSpace(*as) != "" {
		cfg.Offer = &UpdateOffer{
			Version: strings.TrimSpace(*av),
			URL:     strings.TrimSpace(*au),
			SHA256:  strings.ToLower(strings.TrimSpace(*as)),
		}
	}
	return cfg
}

type rawJob struct {
	ID      int64  `json:"id"`
	Kind    string `json:"kind"`
	Payload string `json:"payload"`
}

type pollResponseJSON struct {
	ConfigPayload
	Jobs []rawJob `json:"jobs"`
}

// ---------- rotas ----------

func (c *Client) GetConfig(ctx context.Context) (*DeviceConfig, error) {
	data, err := c.do(ctx, http.MethodGet, "/impressora/config", nil)
	if err != nil {
		return nil, err
	}
	var j ConfigPayload
	if err := json.Unmarshal(data, &j); err != nil {
		return nil, fmt.Errorf("config: JSON inválido: %w", err)
	}
	return j.ToConfig(), nil
}

// Poll devolve a config embutida + jobs (ordem do servidor preservada —
// nunca reordenar, decisão 13). Erro de base64 num job vira erro da rodada:
// sem write, sem ACK — job volta no próximo poll (at-least-once).
func (c *Client) Poll(ctx context.Context) (*DeviceConfig, []Job, error) {
	data, err := c.do(ctx, http.MethodGet, "/impressora/jobs", nil)
	if err != nil {
		return nil, nil, err
	}
	var j pollResponseJSON
	if err := json.Unmarshal(data, &j); err != nil {
		return nil, nil, fmt.Errorf("poll: JSON inválido: %w", err)
	}
	cfg := j.ToConfig()
	jobs := make([]Job, 0, len(j.Jobs))
	for _, rj := range j.Jobs {
		payload, derr := base64.StdEncoding.DecodeString(rj.Payload)
		if derr != nil {
			return nil, nil, fmt.Errorf("poll: job %d com payload base64 inválido: %w", rj.ID, derr)
		}
		jobs = append(jobs, Job{ID: rj.ID, Kind: rj.Kind, Payload: payload})
	}
	return cfg, jobs, nil
}

func (c *Client) Ack(ctx context.Context, jobID int64) (pending int, err error) {
	data, err := c.do(ctx, http.MethodPost, "/impressora/jobs/"+strconv.FormatInt(jobID, 10)+"/ack", nil)
	if err != nil {
		return 0, err
	}
	var r struct {
		Pending int `json:"pending"`
	}
	if err := json.Unmarshal(data, &r); err != nil {
		// 2xx sem JSON é aceitável (contrato manda {"pending": N}, mas um
		// 2xx idempotente inesperado não pode travar o lote).
		return -1, nil
	}
	return r.Pending, nil
}

// ReportPrinters é o passo 2 do boot: snapshot substitutivo das impressoras
// + telemetria (agent_version/os/arch/hostname — o Rails guarda a versão; os
// demais campos são ignorados hoje e aditivos amanhã, epic §7.1).
func (c *Client) ReportPrinters(ctx context.Context, body map[string]any) (stored int, err error) {
	data, err := c.do(ctx, http.MethodPost, "/impressora/printers", mustJSON(body))
	if err != nil {
		return 0, err
	}
	return parseStored(data)
}

// SendLogs é a rodada do debug_mode (decisão 18): {"entries": [...]}.
func (c *Client) SendLogs(ctx context.Context, entries []string) (stored int, err error) {
	body := map[string]any{"entries": entries}
	data, err := c.do(ctx, http.MethodPost, "/impressora/logs", mustJSON(body))
	if err != nil {
		return 0, err
	}
	return parseStored(data)
}

// DownloadToFile baixa o binário do update já para o caminho final (temp do
// swap), calculando o sha256 em voo. Redirects: cada salto tem que continuar
// https quando a base é https (epic §7.2 — redirect cross-host derruba o
// header Authorization: url externa deve vir pré-assinada; o Go derruba o
// header automaticamente, aqui só garantimos o esquema).
func (c *Client) DownloadToFile(ctx context.Context, rawURL, dstPath string) (sha256hex string, err error) {
	// Epic §7.2: url de produção obrigatoriamente https — com base https o
	// agente recusa qualquer download http antes de sair daqui.
	if u, perr := url.Parse(rawURL); perr != nil || (c.HTTPSOnly && (u == nil || u.Scheme != "https")) {
		return "", fmt.Errorf("download recusado: url precisa ser https (veio %q)", rawURL)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+c.token())
	req.Header.Set("X-Agent-Version", Version)
	c.DL.CheckRedirect = func(r *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return fmt.Errorf("muitos redirects no download")
		}
		if c.HTTPSOnly && r.URL.Scheme != "https" {
			return fmt.Errorf("redirect para não-https bloqueado: %s", r.URL)
		}
		return nil
	}
	resp, err := c.DL.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return "", &HTTPError{Status: resp.StatusCode}
	}
	return writeFileHashing(dstPath, resp.Body)
}

func parseStored(data []byte) (int, error) {
	var r struct {
		Stored int `json:"stored"`
	}
	if err := json.Unmarshal(data, &r); err != nil {
		// 2xx sem JSON: não trava o fluxo do chamador.
		return -1, nil
	}
	return r.Stored, nil
}

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		// Corpos do agente são mapas/slices de tipos básicos — marshal falha
		// só por bug nosso; melhor devolver corpo vazio que panicar.
		return []byte("{}")
	}
	return b
}
