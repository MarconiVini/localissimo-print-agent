# Épico S5 — printer-agent (especificação fechada)

> **Fonte da verdade do S5.** O lado Rails (S0–S4) já está implementado no Localíssimo; este
> documento cobre o agente e os acréscimos que o Rails ainda vai receber (telemetria + auto-update).
>
> ⚠️ `README.md` e `config.example.json` deste repo descrevem o **contrato antigo** (2 rotas,
> `config.json` local com token/impressora/intervalo) e serão reescritos quando o S5 aterrissar —
> atualizar junto o `tester/README.md` e **apagar o `config.json` legado da raiz** (tem token da
> era de 2 rotas; o `.gitignore` o protege, mas o arquivo não serve mais). O `tester/index.html`
> já cobre as 5 rotas do contrato atual.
>
> ✅ **S5 aterrissado (2026-11-16).** O agente está implementado neste repo (só stdlib, 3 OS,
> testes em `go test ./...`; releases via `./build.sh` → `dist/` + SHA256SUMS.txt). Os READMEs
> foram reescritos e o `config.json` legado apagado.
>
> ⚠️ **§7.3 superado pelo lado Rails real.** A policy de update (objeto `agent_update` aninhado
> no poll, canary/throttle/withdraw server-side) foi substituída por um desenho mais simples,
> já implementado na branch `feat/comandas-termicas` (2026-11-16): catálogo global
> `printer_agent_releases` (version X.Y.Z + download_url + sha256, publicado no card Comandas do
> admin) + **alvo por negócio** (`printers.target_agent_version`) + **campos flat no payload**
> `agent_version`/`agent_download_url`/`agent_sha256` (null = fica onde está) + feedback
> `agent_version` no POST `/impressora/printers` (→ `reported_agent_version`). Blueprint
> atual: `docs/epicos/comandas-termicas-printer-agent.md` no repo do Localíssimo. O agente
> implementa exatamente esse contrato; canary/janela horária/força-após-N-dias continuam
> refinamentos futuros do lado Rails (§10), e o agente não muda para recebê-los.

---

## 1. Filosofia (não negociável)

1. **Rails é a autoridade de render; o agente é cano burro** (decisão 8) — o payload chega em
   ESC/POS pronto (base64) e é escrito verbatim na impressora. `columns`/`codepage`/`auto_cut`/
   `name` da config são **ignorados** pelo agente — são input do render no Rails.
2. **Config vive no servidor** (decisão 15) — nada de config local com IP/impressora/intervalo.
   O agente é stateless em config.
3. **At-least-once** (decisão 14) — ACK só depois do write bem-sucedido; ACK perdido traz o job
   de volta e reimprime. Aceito: troco reimpresso custa menos que comanda perdida.
4. **Falha de impressora não falha em silêncio** (decisão 9) — sem write, sem ACK; job segue
   `pending`, badge do kanban sobe, agente segue online (heartbeat fresco).
5. **404 indistinto** (ADR-0001) — token inválido, flag `orders.comandas` desligada e job de
   outro negócio dão o mesmo 404. O agente retria — nunca desiste.
6. **Nenhum estado de negócio depende do relógio local** — o servidor carimba tudo
   (`last_seen_at`, `printed_at`, `reported_at`). (Timestamps de entrada de debug são só para
   leitura humana — ver §6.)

## 2. Plataformas (Windows, Linux e macOS)

Binário único em Go, **só stdlib, estático** (`CGO_ENABLED=0`), cross-compilado pelo `build.sh`
para os mesmos 5 alvos de sempre:

| Alvo | Uso |
|---|---|
| `printer-agent-windows-amd64.exe` | Windows do balcão (**alvo primário**, decisão 7) |
| `printer-agent-linux-amd64` | Linux 64-bit |
| `printer-agent-linux-arm64` | Linux ARM 64-bit (ex.: Raspberry Pi OS 64-bit) |
| `printer-agent-darwin-arm64` | Mac Apple Silicon |
| `printer-agent-darwin-amd64` | Mac Intel |

Matriz de comportamento por OS:

| Aspecto | Windows | Linux | macOS |
|---|---|---|---|
| Descoberta de impressoras (boot) | `EnumPrinters` + `EnumPorts` (porta TCP/IP) | `lpstat -p` + `lpstat -v` (CUPS) | idem Linux (CUPS nativo) |
| Writer `usb` | Spooler RAW (`OpenPrinter` → `StartDocPrinter` level 1 `"RAW"` → `WritePrinter` → `EndDocPrinter` → `ClosePrinter`) | fila RAW do CUPS via `lp` | fila RAW do CUPS via `lp` |
| Writer `network` | TCP RAW `host:port` | igual | igual |
| Instância única | mutex nomeado (retry ~30s pós-update) | `flock` em `agent.lock` (mesmo retry) | idem Linux |
| Auto-start no boot | atalho `shell:startup` **ou** `schtasks /sc onlogon` | autostart `.desktop` ou unidade `systemd --user` | LaunchAgent (`KeepAlive`) |
| Log | `agent.log` rotacionado na pasta | igual | igual |
| Swap do auto-update | rename dance (renomear exe rodando é permitido) | mesmo fluxo (rename é atômico) | idem |

Notas por plataforma:

- **Windows**: o binário baixado pelo auto-update não ganha Mark-of-the-Web (Go escreve o arquivo
  cru), então SmartScreen normalmente não interfere; antivírus pode escanear o binário novo no
  spawn (atraso de segundos é esperado), mas pode também **quarentená-lo/removê-lo antes do
  `CreateProcess`** — spawn falho é falha de update com rollback (§7.4), não atraso. Codesign é
  melhoria futura, não requisito v1.
- **Linux/macOS**: writer `usb` exige fila CUPS RAW apontada pro device (criada uma vez:
  `lpadmin -p POS80 -E -v usb://… -m raw`). O payload vai no **stdin** de `lp -d <fila> -o raw`;
  exit 0 = sucesso (tudo-ou-nada), exit ≠ 0 = falha (stderr vai pro log/entrada de debug).
  Servidor headless sem CUPS → só writer `network`. Em dev sem permissão de `lp`, a ponte
  TCP:9100 → fila CUPS RAW (padrão do ambiente local) continua valendo.
- **macOS**: quarantine/Gatekeeper vale só para binário copiado/baixado na mão (`xattr -c` na
  instalação manual). O download do auto-update não cria quarantine.
- **Supervisores com auto-restart** (LaunchAgent `KeepAlive`, `systemd Restart=always`): a
  corrida pós-swap é benigna — velho e novo competem pelo mesmo binário e só um fica (detalhe e
  regra do lock em §7.4).

## 3. Identidade e arquivos ao lado do binário

Tudo relativo à pasta do binário (portável nos 3 OS):

- **`token.txt`** — o token do `print_device` (**não** vai mais em config). Conteúdo: exatamente
  o token (trim). Ausente/vazio → instrução clara no log + retry em 30s (o arquivo pode chegar
  depois; **nunca sai**). Lido **só no boot** — troca de token = editar o arquivo e reiniciar
  (até lá, 404 em tudo, retry eterno).
- **`config.json`** — opcional, só `{"server": "..."}` sobrescrevendo o default compilado.
  **Precedência: default compilado < `config.json` < flag `-server`** (dev/tester aponta pro
  localhost). Nada mais dentro.
- **`agent.log`** — log local rotacionado (~1 MB, mantém 1 anterior), porque rodando como
  tarefa/serviço não há console. Sempre ativo, independente do debug_mode. Rotação =
  **fecha → renomeia → reabre**; rename falhou (arquivo preso, ex. Windows) → segue escrevendo
  no mesmo arquivo e tenta de novo no boot seguinte.
- **`agent.lock`** — lock de instância única (Linux/macOS; Windows usa mutex nomeado). Liberado
  pelo kernel em crash — lock órfão é inofensivo.
- **`printer-agent.download`** — binário em download/confirmação do auto-update (temporário;
  escrito já com **0755** — chmod no POSIX, no-op no Windows).
- **`printer-agent.old`** — lixo do swap do auto-update, apagado no boot seguinte
  (apagada que falha — ex.: arquivo ainda mapeado — é tolerada e tentada de novo no boot seguinte).

## 4. Boot em 4 passos (decisão 17, contrato fechado em 2026-11-15)

1. **Lê `token.txt`** — sem identidade não há o que fazer: retry lento com instrução no log.
2. **Reporta as impressoras** — `POST /impressora/printers` (capacidade nova). Antes de qualquer
   outra coisa, inspeciona as impressoras do OS (matriz da seção 2) e manda:
   ```json
   {
     "agent_version": "0.4.0",
     "os": "windows",
     "arch": "amd64",
     "hostname": "BALCAO-01",
     "printers": [
       { "name": "POS-80", "PortaNome": "IP_192.168.0.50", "Host": "192.168.0.50", "Porta": "9100", "driver": "POS-80 Printer" }
     ]
   }
   ```
   - **`Porta`** = porta TCP numérica **como string** (`"9100"`); **`PortaNome`** = nome da porta
     no OS quando existir (`IP_192.168.0.50`, `USB001`); **todos os valores do report são
     strings**. No Windows `Host`/`Porta`/`PortaNome` vêm do `EnumPorts` (o `EnumPrinters`
     sozinho só dá o nome da porta); no Linux/macOS saem do URI `socket://host:port` do
     `lpstat -v` (porta desconhecida → omitir `Porta`).
   - **`hostname`** da máquina vai junto — o painel exibe; é o único sinal de **pasta clonada**
     (mesmo `token.txt` em 2 máquinas = todo job sai **em dobro**, sem outro aviso no admin).
   - O agente **filtra** impressoras de arquivo/PDF/XPS (`PORTPROMPT:`, `FILE:`, Microsoft Print
     to PDF/XPS) e ordena por heurística de térmica (driver/USB/TCP RAW) **antes** do teto de 50 —
     senão o corte pode excluir justamente a térmica.
   - Snapshot **substitutivo** no servidor (o novo vence); os nomes alimentam o datalist
     "Impressora no Windows" do painel admin.
   - **Gate**: o passo 3 só começa após o 1º POST bem-sucedido; falhou, backoff exponencial
     (base do intervalo, teto 60s) e repete.
3. **`GET /impressora/config`** (Bearer) — só com `configured=true` entra no loop;
   `configured=false` (admin ainda não completou a impressora) → fica reconsultando
   (conta heartbeat) sem escrever em impressora nenhuma. 404 → retry em **30s fixo** (sem
   backoff exponencial; 404 não conta heartbeat — device com a flag desligada fica vermelho no
   card até ligarem, e é esperado); quando ligarem a flag ou trocarem o token, ele volta sozinho.
4. **Loop de execução** (seção 5).

## 5. Loop: poll → write → ack

- **Poll** `GET /impressora/jobs` devolve a config completa embutida + `jobs: [{id, kind, payload}]`.
  - **Ordem garantida pelo servidor** (mais antigo primeiro, cozinha antes da guia do mesmo
    pedido — decisão 13): **preservar, nunca reordenar**.
  - **Todo poll bem-sucedido é heartbeat** — inclusive o vazio; >1 min sem poll = ponto vermelho
    no card do owner.
  - Dorme `poll_interval` da config — **ausente/≤0 → default 3s**, depois **clamp [1s, 60s]** —
    com ou sem jobs.
- **Aplicação de config nova**: o poll **já embute a config completa** — `config_version`
  (ISO8601) mudou → o agente aplica direto do poll (conexão/host/porta/impressora/intervalo/
  debug) **sem reiniciar o processo**. A aplicação acontece **só entre rodadas**: o lote em
  curso termina com a config vigente (aceito); a config nova vale a partir do poll seguinte.
  `GET /impressora/config` fica para o boot e o loop do `configured=false`.
- **`configured=false` em qualquer resposta** (boot ou config embutida no poll — ex.: admin
  desfaz a impressora no meio do dia) → **congela a escrita**: nenhum write, nenhum ACK; segue
  pollando (heartbeat); volta sozinho quando `configured=true`.
- **Dois writers, escolhido por `connection` por job** (troca rede↔USB no painel vale no poll
  seguinte):
  - `network`: TCP RAW `host:port` (port null → 9100), dial ~5s, write ~10s, close. **Sucesso =
    payload completo escrito** (`n == len(payload)`, loop de write); `n < len` ou erro = falha,
    sem ACK — comanda pela metade não existe.
  - `usb`: por OS (matriz da seção 2), pelo `printer_name` exato — tudo-ou-nada (retorno ok =
    tudo escrito).
- **ACK** `POST /impressora/jobs/:id/ack` **só após write ok**; resposta `{"pending": N}`
  (loga — é o número do badge).
  - ACK falhou (5xx/timeout) → loga e **segue o lote** (at-least-once: o job volta no próximo poll).
  - **4xx no ACK** (job já `printed` por outro canal — ex. modo browser do owner — ou ack
    duplicado do redelivery; o Rails responde **2xx idempotente**, §9) → loga e segue o lote,
    sem backoff e sem reenviar.
  - **Write falhou → quebra o lote, sem ACK** (evita N dials ruins), job segue `pending`,
    próximo poll tenta de novo.
- **Backoff só em falha de poll/rede/servidor (5xx/timeout)**: exponencial do intervalo até 60s,
  reset no sucesso. **404 → 30s fixo** (regra do passo 3). **Falha de impressão não afasta o
  poll** — o heartbeat tem que ficar fresco.

## 6. Estado em memória e modo debug (decisão 18)

O agente mantém **em memória** (uma variável — buffer circular com teto do contrato do servidor:
200 entradas × 2000 chars; estourou → descarta as mais antigas) o log de todas as ações de cada
rodada:

- timestamp (ISO8601 UTC) de cada evento;
- poll: jobs recebidos (ids), config_version vigente;
- por job: writer escolhido, resultado (bytes escritos / erro);
- ACK ok/falho + `pending` devolvido;
- troca de `config_version` e campos aplicados;
- tentativa de auto-update (oferta recebida, download, sha, swap, versão nova);
- erros de rede (dial, timeout, 5xx, backoff armado).

Regras do conteúdo:

- Estouro de 2000 chars trunca a **mensagem** preservando o timestamp e mantendo a **cauda**
  (em erro de rede, a causa costuma estar no fim).
- **Entradas jamais contêm payload/comanda, token ou dados de cliente** (nome, telefone,
  endereço) — só ids, contadores, bytes escritos e mensagens de erro de infra.
- O timestamp das entradas é só para leitura humana; carimbo de negócio é sempre server-side (§1.6).

**`debug_mode=true`** → ao fim de cada rodada (poll + lote da rodada — **não** um POST por job)
envia `POST /impressora/logs` com `{"entries": [...]}`; resposta `{"stored": N}` é logada.
**POST de log falhou → engole** (nunca vira segunda fila).

**`debug_mode=false`** → nenhum POST; o buffer continua sendo mantido em memória (mesmo teto —
se o admin ligar o debug ao vivo, a rodada seguinte já sai com o histórico recente) e o
`agent.log` local continua sempre.

## 7. Telemetria e auto-update

### 7.1 Telemetria (pré-requisito de qualquer rollout)

- O agente manda `X-Agent-Version` em **toda request autenticada**, e `agent_version`/`os`/`arch`/
  `hostname` no report do boot (seção 4).
- Rails grava `agent_version`, `agent_version_at`, `agent_os`, `agent_arch`, `agent_hostname` no
  `print_device`.
- O card/painel mostra "agente 0.3.0 · desatualizada" vs "0.4.0 · atual" e o hostname. Sem isso
  não há visibilidade do que está instalado em cada balcão.

### 7.2 Catálogo de releases no Rails

Tabela `agent_releases`: `version`, `platform` (GOOS/GOARCH), `sha256`, `url` (attachment
ActiveStorage ou rota do próprio Rails — ~10 MB por binário, sem CDN), `state`
(`canary` / `active` / `withdrawn`), `released_at`. Registro por rake task ou form admin com os
hashes saídos do `build.sh`.

- **Validação no registro**: rodar o binário alvo com `-version` e conferir igualdade com a
  versão registrada + recalcular o sha256 — release mal registrado (ldflags esquecido, sha
  digitado errado) vira **ciclo eterno de download/swap/restart** por device, invisível.
- **Download autenticado**: `Authorization: Bearer` do device no **header** do GET — **nunca**
  token em query string (vaza em access log). `url` de produção obrigatoriamente **https**;
  redirects seguidos só se cada salto continuar https (redirect cross-host derruba o header
  `Authorization` — url externa deve vir **pré-assinada** e dispensar o header). Token inválido
  → recusa.

### 7.3 Decisão server-side (a policy vive num lugar só)

A resposta do **poll** ganha um objeto **opcional** `agent_update` — ausente = "fica onde está":

```json
{
  "jobs": [],
  "config_version": "...",
  "agent_update": { "version": "0.4.0", "url": "https://…/printer-agent-windows-amd64.exe", "sha256": "…" }
}
```

A policy anexa quando **tudo** vale:

1. existe release `active` (ou `canary`, se o device tem `update_channel=canary`) da plataforma
   `(agent_os, agent_arch)` reportada — **device sem telemetria nunca recebe oferta automática**
   (rollout manual) — com `version` **diferente** da reportada;
2. device **idle**: `pending == 0` (o Rails conta os drainables) — update nunca atrasa comanda;
3. `auto_update` ligado na config do device (flag pra dev/teste desligar);
4. throttle: `update_offered_at` do device mais velho que N horas (evita re-oferta a cada poll) —
   **mas o throttle não se aplica a downgrade** (alvo anterior ao reportado; o Rails sabe a
   direção pelo catálogo) e a transição de uma release para `withdrawn` **zera
   `update_offered_at`** dos devices que a receberam — withdraw é emergência, não passa na régua
   do anti-spam.

Ademais: Rails **alerta no admin** após K re-ofertas sem mudança de versão reportada (ciclo
eterno de update fica visível).

**Comparação por igualdade de string** — sem semver em lugar nenhum do cliente; o agente só
pergunta "minha versão == alvo?" (o Rails pode ordenar no catálogo — é server-side). **Withdraw
vira downgrade de graça**: o agente não sabe a direção, só recebe "sua versão alvo é X" e
obedece. A policy usa só o que o Rails tem que nenhum feed externo teria: idle, horário (janela
é refinamento futuro), histórico de falhas.

### 7.4 Aplicação no agente (a parte espinhosa é Windows — o Windows não deixa sobrescrever um .exe em execução)

A versão vem de `-ldflags` no `build.sh` e é exposta via `-version`; build "sujo" = não casa
`^v?\d+\.\d+\.\d+$`.

1. Poll veio **vazio** e trouxe `agent_update` com `version !=` própria (build "sujo" de dev
   **ignora** e loga o motivo);
2. Se `printer-agent.download` já existe com o sha256 esperado → **pula o download**; senão
   baixa **em background/concorrente ao poll** (com timeout ~5 min; falhou → descarta e
   re-baixa no próximo ciclo) para `printer-agent.download`, já com **0755**;
3. Confere sha256 — falhou → descarta, loga (entra no buffer de debug), segue na atual.
   **Update nunca pode quebrar a impressão.**
4. Numa rodada com **poll vazio e download pronto** — pronto = o `(version, sha256)` guardados
   da oferta que originou o download **iguais à última oferta recebida** (alvo mudou no meio —
   withdraw/troca — → descarta `.download` e re-baixa; nunca instala release retirada):
   rename do binário em execução → `printer-agent.old` → rename do `.download` → binário →
   **spawn do novo** (caminho absoluto via `os.Executable()`) com env `AGENT_UPDATE_RESTART=1`.
   **Spawn confirmado é condição para encerrar**: falhou (AV removeu, disco, permissão) →
   **reverte os renames** (`.old` volta a binário), loga e segue na versão atual.
5. A instância nova, ao ver a env, faz **retry do lock/mutex por até ~30s** (o processo velho
   pode ainda estar vivo); **não adquiriu o lock → exit com erro claro** — nunca segue sem lock
   (dois agentes = todo job sai em dobro); quem religa é o autostart/supervisor. A nova faz o
   boot normal (token na pasta não é tocada pelo swap) e o primeiro report/poll dela carrega a
   versão nova — **é assim que o Rails confirma que o update deu certo**.

**Self-heal no boot**: binário principal sumiu (crash entre os dois renames) e `printer-agent.download`
existe → renomeia e segue. Device que sumiu depois de update = badge vermelho no admin
(mecanismo de saúde é o que já existe: heartbeat + versão reportada). `.old` fica de rollback manual.

## 8. Requisitos operacionais

- **Auto-start no boot** por OS (matriz da seção 2) — documentar os dois caminhos do Windows.
- **Instância única** (mutex nomeado no Windows; `flock` no Linux/macOS) com a graça de retry
  pós-update e saída com erro se o lock não vier (nunca duplicar instância).
- **Log rotacionado** local (~1 MB, 1 anterior) sempre ativo, com fallback se o rename falhar.
- **Shutdown limpo** no Ctrl+C/SIGTERM/fechar-janela (CTRL_CLOSE_EVENT) — pendentes ficam no
  servidor, nada se perde. (`taskkill` sem `/F` não entrega sinal a console app; `taskkill /F`
  é hard-kill e pula o shutdown limpo — pendentes ficam no servidor, aceito.)
- **Só https em produção; nunca desligar verificação de TLS** — inclusive no download do
  update (§7.2).
- **Nada de negócio depende do relógio local** — o servidor carimba tudo.

## 9. Superfície HTTP (contrato congelado do S4 + acréscimos aditivos)

| Rota | Método | Request | Response | Quando |
|---|---|---|---|---|
| `/impressora/printers` | POST | `{"agent_version","os","arch","hostname","printers":[{name,PortaNome,Host,Porta,…}]}` | `{"stored": N}` | boot, passo 2 — antes de tudo |
| `/impressora/config` | GET | — | config completa | boot + loop do `configured=false` |
| `/impressora/jobs` | GET | — | config + `jobs:[{id,kind,payload}]` (+ `agent_update` opcional) | cada poll (heartbeat) |
| `/impressora/jobs/:id/ack` | POST | — | `{"pending": N}` | após cada write ok |
| `/impressora/logs` | POST | `{"entries":[…]}` | `{"stored": N}` | cada rodada com `debug_mode` |

- `Authorization: Bearer <token>` em todas (esquema case-insensitive); toda request autenticada
  atualiza `last_seen_at`; `Content-Type: application/json` nos POSTs.
- **Tipos cravados**: `payload` = base64 **std com padding** (equivalente a `btoa`/
  `StdEncoding`); `id` inteiro; `kind` = `"kitchen" | "delivery"`; valores do report, todos strings.
- Tetos do servidor (não adianta exceder): report **50 impressoras × 20 chaves × 40/500 chars**;
  logs **200 entradas × 2000 chars**.
- Download do binário (novo): `Authorization: Bearer` no header (§7.2); token inválido → recusa.
- **Acréscimos Rails**: ACK **idempotente** (ack repetido ou de job já `printed` do mesmo
  negócio → 2xx com o `pending` atual); **`poll_interval` saneado na emissão** (ausente/≤0 → 3s,
  depois clamp [1,60] server-side).

## 10. O que falta no Rails (acréscimos do S5)

- **Telemetria**: colunas `agent_version`/`agent_version_at`/`agent_os`/`agent_arch`/
  `agent_hostname` no `print_device`; aceitar os campos novos no POST `/impressora/printers` e o
  header `X-Agent-Version` no poll.
- **Catálogo**: tabela `agent_releases` + forma de registro com validação (`-version` do binário
  alvo + sha recalculado) + rota de download autenticada (Bearer no header, https).
- **Policy de update**: `agent_update` na resposta do poll (idle + versão + `auto_update` +
  throttle + canal), `update_channel` e `update_offered_at` no device; **withdraw zera
  `update_offered_at`**; **downgrade ignora throttle**; **alerta após K re-ofertas sem mudança
  de versão**.
- **Contrato**: idempotência do ack; saneamento/clamp server-side do `poll_interval`;
  `hostname` no device e no painel.
- Tudo é **aditivo** — nenhuma das 5 rotas muda de forma que quebre o contrato congelado.
- Decisões fechadas (v1): **sem assinatura ed25519** (HTTPS + sha256; trade-off documentado:
  auto-update eleva o Rails de "pode imprimir" para "pode executar código no PC do balcão" —
  assinatura entra se um dia isso incomodar); **canary** = o balcão do Marcos primeiro, observa
  ack rate/last_seen por alguns dias, promove; janela horária e "força update após N dias"
  (restaurante 24h nunca fica idle) são refinamentos posteriores.

## 11. Sequência de implementação

1. **Agente core nos 3 OS** — boot/report → config → loop poll/write/ack → buffer de debug;
   roda contra o S4 existente sem nenhuma mudança no Rails.
2. **Telemetria** — header + campos do report; colunas + badge no Rails. Visibilidade primeiro.
3. **Auto-update** — `agent_releases` + `agent_update` no poll + download/swap/restart no agente
   (com rollback de spawn, revalidação de oferta e regras de throttle/withdraw da §7).
4. **Refinamentos** — canary/withdraw fino, janela horária, força-após-N-dias.

## 12. Desenvolvimento e teste

- `tester/index.html` exercita as 5 rotas com payload editável contra o Rails de dev.
- Sem hardware USB direto: ponte TCP:9100 → fila CUPS RAW (padrão do ambiente local, POS-80).
- Testes unitários sem hardware: fake Rails via `httptest` + fake impressora TCP (herdado do
  README antigo), mais: parsing do report por plataforma (fixtures de `lpstat`/EnumPrinters),
  buffer de debug (teto, truncamento com cauda preservada), write completo (`n < len` = falha),
  política de swap em temp dir **incluindo rollback de spawn falho e revalidação da oferta**,
  retry de lock pós-update, rotação do `agent.log`, filtros do report (PDF/XPS fora,
  térmica primeiro).
