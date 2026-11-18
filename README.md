# printer-agent (S5 das Comandas Térmicas)

Agente de impressão do épico **Comandas Térmicas 80mm** — a especificação
fechada vive no `epic.md` deste repo (e em
`docs/epicos/comandas-termicas-printer-agent.md` no repo do Localíssimo, que
documenta o lado Rails já implementado). Programa único em Go, **só stdlib,
estático** (`CGO_ENABLED=0`), para Windows do balcão (alvo primário), Linux e
macOS.

Filosofia não negociável: **o Rails é a autoridade de render; o agente é cano
burro.** O payload chega em ESC/POS pronto (base64) e é escrito verbatim na
impressora — `columns`/`codepage`/`auto_cut` são ignorados aqui. Ajuste de
comanda nunca exige recompilar o agente.

## Como funciona (epic §3–§5)

**Boot em 4 passos** — tudo ao lado do binário (portável nos 3 OS):

1. Lê **`token.txt`** (o token do `print_device`, trim; ausente/vazio →
   instrução no log + retry em 30s — o arquivo pode chegar depois, o agente
   nunca sai; re-lido a cada retry: trocar token = editar o arquivo).
2. Reporta as impressoras — `POST /impressora/printers`: EnumPrinters +
   EnumPorts + registry TCP/IP no Windows; `lpstat -p`/`-v` no Linux/macOS.
   Filtra PDF/XPS/PORTPROMPT/FILE, ordena térmica primeiro e corta no teto
   de 50. Carrega `agent_version`/`os`/`arch`/`hostname` (telemetria). Este
   passo **gateia o boot**: só segue após o 1º POST bem-sucedido.
3. `GET /impressora/config` — só com `configured=true` entra no loop.
   `configured=false` → reconsulta (conta heartbeat) sem escrever em
   impressora nenhuma. 404 (token inválido ou comandas desligadas) → retry
   em **30s fixo** e volta sozinho quando resolvido.
4. **Loop** `GET /impressora/jobs` (poll) → write → `POST /impressora/jobs/:id/ack`:
   - ordem do lote é do servidor — **preservada, nunca reordenada**;
   - **todo poll é heartbeat** (inclusive o vazio; >1 min = ponto vermelho);
   - dorme `poll_interval` da config (ausente/≤0 → 3s; clamp [1s, 60s]);
   - **write completo ou nada**: TCP RAW com loop de write (`n < len` =
     falha) ou spooler/fila RAW tudo-ou-nada; write falhou → **sem ack,
     quebra o lote**, job segue `pending`, badge sobe, agente segue online;
   - ack só depois do write; ack falhou → loga e segue o lote
     (**at-least-once**: job volta e reimprime);
   - `config_version` mudou → aplica entre rodadas **sem reiniciar**;
   - backoff exponencial só em falha de poll/rede/servidor (até 60s, reset
     no sucesso); falha de impressão **não** afasta o poll.

**Arquivos ao lado do binário** (epic §3): `token.txt` · `config.json`
opcional só com `{"server": "..."}` (precedência: default compilado <
`config.json` < flag `-server`) · `agent.log` rotacionado (~10 MB, 1
anterior; sempre ativo, com TUDO — evento + espelho do buffer de debug;
o envio ao servidor só com `debug_mode` ligado no painel) · `agent.lock` (instância única: flock/mutex
nomeado) · `printer-agent.download` (binário do auto-update) ·
`printer-agent.old` (lixo do swap, apagado no boot seguinte).

## Auto-update (epic §7; lado Rails já implementado)

O card Comandas do admin publica versões no catálogo `printer_agent_releases`
(version X.Y.Z + download_url + sha256) e escolhe o alvo por negócio
(`printers.target_agent_version`). O payload da config/poll embute os 3
campos (`agent_version`/`agent_download_url`/`agent_sha256`) — null = fica
onde está.

O agente, em **rodada idle** (poll vazio — nunca atrasa comanda):

1. compara a versão alvo com a própria (por igualdade de string, zero
   semver no cliente); build de dev (`0.0.0-dev`) **ignora e loga**;
2. baixa para `printer-agent.download` (já 0755; arquivo pré-existente com o
   sha esperado → pula o download; timeout ~5 min; falha → re-baixa na
   próxima rodada idle);
3. confere **sha256** — divergiu → descarta e segue na atual;
4. oferta mudou/sumiu no meio → descarta o download velho (nunca instala
   release retirada);
5. swap: binário em execução → `printer-agent.old` → `.download` → binário →
   **spawn do novo** (env `AGENT_UPDATE_RESTART=1`). Spawn falhou → reverte
   os renames e segue na atual. Spawn confirmado → processo velho encerra;
   com `-spawnless` (serviço) não há spawn: troca concluída → encerra com
   falha e o supervisor (SCM/systemd) religa no binário novo;
6. a instância nova espera o lock até ~30s (o velho pode ainda estar vivo);
   sem lock → **exit com erro** (nunca duas instâncias = todo job em dobro).
   O 1º report/poll dela carrega a versão nova — é assim que o Rails
   confirma o update.

**Self-heal no boot**: binário sumido (crash entre renames) + `.download`
existe → renomeia e segue; `.old` fica de rollback manual.

## Instalação

### Windows do balcão (alvo primário)

1. Crie `C:\localissimo-print-agent\` e copie o
   `printer-agent-windows-amd64.exe` para lá (renomeie para
   `printer-agent.exe`, se quiser — qualquer nome funciona).
2. Na mesma pasta, crie o **`token.txt`** com o token do device: no card
   Impressora do quadro de pedidos, "Conectar agente no Windows" cria/reusa
   o device "Agente no balcão" e mostra o token com copiar.
3. Teste na mão (Prompt de Comando **nessa pasta**):

   ```bat
   printer-agent.exe
   ```

   Log esperado: versão, servidor, report de impressoras, config. Dispare
   uma comanda de teste pelo kanban e confira na impressora.
4. **Serviço no boot (recomendado — pacote v0.2.0+):** clique com o direito
   no `install.bat` da pasta `windows\` do pacote → "Executar como
   administrador". O agente vira o serviço **"Localissimo Printer Agent"**
   (`printer-agent` no SCM): sobe no boot **sem login** e, se o processo
   cair, o próprio Windows religa em ~10s (`sc failure` + `failureflag`,
   configurados pelo instalador) — **não há watcher: quem vigia é o OS**.
   O instalador roda o binário com `-spawnless`: no auto-update ele não
   spawna filho — encerra com falha e o SCM religa já no binário novo.
   Desinstalar: `uninstall.bat` (arquivos preservados). O agente continua
   **portável**: tudo (token, log, lock, download do update) vive na pasta
   dele — mover a pasta = mover o agente inteiro (reinstale na pasta nova).
5. Atualizar de versão: em produção, o próprio agente se atualiza pelo
   auto-update (alvo no card Comandas) — instalado como serviço, o SCM
   religa na versão nova. Via pacote: copie a pasta da versão nova ao lado
   e nela rode o `install.bat` (recria o serviço apontando para a pasta
   nova; rollback = rodar o install da pasta antiga).

### Linux amd64 / Raspberry Pi OS 64-bit (linux-arm64)

Com o **pacote** (`dist/v<versão>/linux/`), um comando registra o serviço
systemd — detecta amd64/arm64 pelo `uname -m`, sobe no boot **sem login** e
religa em ~10s se o processo cair (`Restart=on-failure`, `StartLimitIntervalSec=0`):

```bash
sudo ./install.sh
```

Diagnose: `systemctl status printer-agent` · `journalctl -u printer-agent -f`
· `tail -f agent.log`. Desinstalar: `sudo ./uninstall.sh` (arquivos
preservados). Manual (sem o pacote):

```bash
sudo mkdir -p /opt/localissimo
sudo cp printer-agent-linux-amd64 /opt/localissimo/printer-agent
sudo chmod +x /opt/localissimo/printer-agent
echo "SEU_TOKEN" | sudo tee /opt/localissimo/token.txt
/opt/localissimo/printer-agent   # teste na mão
```

Serviço com auto-restart — `/etc/systemd/system/localissimo-print-agent.service`:

```ini
[Unit]
Description=Localissimo printer-agent
After=network-online.target
Wants=network-online.target
StartLimitIntervalSec=0

[Service]
WorkingDirectory=/opt/localissimo
ExecStart=/opt/localissimo/printer-agent -spawnless
Restart=on-failure
RestartSec=10s

[Install]
WantedBy=multi-user.target
```

```bash
sudo systemctl daemon-reload && sudo systemctl enable --now localissimo-print-agent
journalctl -u localissimo-print-agent -f     # e/ou: tail -f /opt/localissimo/agent.log
```

Writer `usb` no Linux/macOS exige fila CUPS RAW criada uma vez:
`lpadmin -p POS80 -E -v usb://… -m raw` (o payload vai no stdin de
`lp -d POS80 -o raw`). Headless sem CUPS → use conexão `network`.

### macOS (Apple Silicon: darwin-arm64 · Intel: darwin-amd64)

1. Copie o binário para `~/localissimo/printer-agent` e `chmod +x`.
2. Binário baixado/copiado na mão → limpar quarentena: `xattr -c printer-agent-*`.
   (O download do **auto-update não** cria quarantine.)
3. `echo "SEU_TOKEN" > ~/localissimo/token.txt` e teste `./printer-agent`.
4. LaunchAgent com `KeepAlive` —
   `~/Library/LaunchAgents/localissimo.print-agent.plist`:

```xml
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN"
  "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
  <key>Label</key><string>localissimo.print-agent</string>
  <key>ProgramArguments</key>
  <array><string>/Users/SEU_USUARIO/localissimo/printer-agent</string></array>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
</dict></plist>
```

```bash
launchctl load ~/Library/LaunchAgents/localissimo.print-agent.plist
```

### Testar a impressora antes (qualquer ambiente)

```bash
# Linux / macOS — conexão aceita = impressora viva
echo | nc 192.168.0.50 9100
```

```powershell
# Windows
Test-NetConnection 192.168.0.50 -Port 9100
```

## Compilando do fonte (máquina de dev)

Pré-requisito: **Go 1.27+**. Em Windows use Git Bash/WSL para o `build.sh`.

```bash
# testes (fake Rails via httptest + fake impressora TCP — sem hardware;
# vivem no pacote externo tests/, que importa internal/agent)
go test ./...
go test ./... -race
go test ./... -coverpkg=./... -cover   # cobertura contra o código real

# build de dev (versão suja: auto-update desligado por segurança)
go build -o printer-agent .

# release em pacote versionado: dist/v0.2.0/{windows,linux,darwin}/
# (binários + instaladores + token.txt vazio + SHA256SUMS.txt — fonte dos
# hashes do catálogo do Rails, epic §7.2)
./build.sh 0.2.0
```

As releases ficam dentro do projeto, em **`dist/v<versão>/`** (gitignored —
binário não vai pro git): cada versão é uma cópia completa com os
instaladores (`install.bat`/`uninstall.bat`, `install.sh`/`uninstall.sh`,
copiados de `packaging/`) e um `LEIA-ME.txt` para a loja. Sem `config.json`
no pacote: a URL de produção é o default compilado do agente
(`internal/agent.DefaultServer`); `-server`/`config.json` seguem como
override dev/tester. Versões antigas nunca são apagadas. O `build.sh`
valida `-version` do binário nativo contra a versão pedida (release mal
registrada vira ciclo eterno de update — epic §7.2). A única dependência
externa do Go é `golang.org/x/sys` (canônica do time Go), confinada ao
harness de serviço do Windows.

## Comportamento (mapa rápido)

| Situação | O agente faz | O dono/suporte vê |
|---|---|---|
| Poll ok, sem jobs | dorme o intervalo (heartbeat) | "conectada · há Xs", fila 0 |
| Impressora morta | write falha, **sem ack**, quebra o lote | badge de pendentes sobe; agente segue online |
| Servidor inalcançável | backoff exponencial (até 60s) | "offline desde HH:MM" |
| Ack perdido | loga e segue; job reimprime | (at-least-once, aceito) |
| Token inválido / flag off | 404 → retry fixo 30s, re-lê token.txt | volta sozinho quando resolvido |
| `configured=false` | congela escrita, segue pollando | admin completa a config → entra no loop sem restart |
| Versão nova anunciada | idle → baixa → sha → swap → restart | card mostra "agente X · em dia" |
| Processo cai (com serviço instalado) | — | SCM (Windows) / systemd religa em ~10s |
| Ctrl+C / SIGTERM | encerra limpo | pendentes ficam no servidor, nada se perde |

Modo debug (`debug_mode` na config): a cada rodada o agente posta o buffer
de eventos (`POST /impressora/logs`, 200×2000 chars, teto do contrato;
timestamp preservado e **cauda mantida** no truncamento; nunca contém
payload/dados de cliente). Falha do POST é engolida — debug nunca vira
segunda fila. O `agent.log` local continua sempre, debug ligado ou não.

## O que os testes cobrem (sem hardware)

`go test ./...` — pacote `tests/` com fake Rails via `httptest` espelhando o
`PrintDevicesController` (404 indistinto, ack idempotente, config embutida
no poll) + fake impressora TCP:

- header Bearer + `X-Agent-Version` em toda rota; esquema case-insensitive;
- parsing da config (nulls, `agent_*` → oferta só com os 3 campos) e do
  poll (base64 std com padding, ordem preservada, payload corrompido =
  erro de rodada);
- boot na ordem (report antes de config), gates de 404/backoff,
  `configured=false`;
- rodada feliz (write completo → ack na ordem), write falho (sem ack, lote
  quebrado, poll segue), ack falho (segue o lote), 404 fixo 30s, backoff
  exponencial com teto/reset, config nova entre rodadas, clamp do intervalo;
- debug: um POST por rodada com o buffer inteiro, falha engolida, sem
  payload nas entradas; buffer: anel de 200, truncamento com cauda;
- log: rotação ~10 MB com fallback de rename falho (arquivo novo abre com
  marcador de rotação); espelho do buffer de debug no arquivo (envio ao
  servidor continua gateado por debug_mode); heartbeat de idle a cada 5 min
  com os números da sessão; estados estáticos (configured=false, 404, token
  ausente) logam 1× e repetem só a cada 5 min com contagem de suprimidas;
  lote marcado antes do write (bracket de crash) e resumo no encerramento;
- update: build sujo ignora, fluxo feliz com swap+spawn, update só idle,
  sha divergente descarta, oferta mudou/sumiu descarta, spawn falho
  reverte, pulo de download com sha válido, self-heal, cleanup do `.old`;
- lock exclusivo + retry pós-update; writer network (payload completo,
  escrita parcial = falha, defaults 9100).

## Layout

```
main.go                  bootstrap: flags, console/serviço (SCM), boot + loop
internal/agent/          TODO o agente (pacote testável, 0 dependências):
  config.go              token.txt / config.json / -server (precedência) + caminhos
  client.go              contrato HTTP: config/poll/ack/printers/logs + download
  agent.go               boot em 4 passos + loop poll→write→ack (epic §4–§5)
  writer_network.go      TCP RAW JetDirect (dial 5s, write 10s, loop completo)
  printer_windows.go     EnumPrinters/EnumPorts/registry + spooler RAW (Windows)
  printer_unix.go        lpstat (CUPS) + lp -o raw (Linux/macOS)
  report.go              filtros/ordenação do report + parsers (puro)
  update.go              auto-update: oferta→download→sha→swap→spawn (+rollback)
  debug.go               buffer de debug 200×2000 (anel, cauda preservada)
  logger.go              agent.log rotacionado
  lock_unix/windows.go   instância única (flock / mutex nomeado)
  version.go             versão do build (ldflags) + validação X.Y.Z
internal/winsvc/         serviço nativo do Windows (x/sys windows-only): o SCM
                         sobe no boot e religa em falha — não há watcher
packaging/               install/uninstall (bat + sh) e LEIA-ME — o build.sh
                         copia pro pacote; os .sh registram unit systemd
tests/                   pacote externo de testes (fake Rails + fake impressora
                         TCP + servidor de binários fake — sem hardware)
tester/                  HTML que simula o device e renderiza o papel 80mm
build.sh                 pacote por versão → dist/v<versão>/{windows,linux,darwin}/
epic.md                  a especificação (fonte da verdade)
```
