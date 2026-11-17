# Tester das Comandas Térmicas

HTML único (sem dependências, abre direto do disco — `file://`) que simula o
**device impressor** do épico (decisões 7, 12, 17 e 18): os mesmos requests
do agente Go, mas **nada vai para impressora** — o payload ESC/POS que o
servidor devolve é interpretado aqui e desenhado como papel 80mm.

Abrir: `tester/index.html` no navegador (dois cliques resolve).

## Os 5 endpoints do contrato (S4 + boot do agente)

| Request | O que faz no tester |
|---|---|
| `POST /impressora/printers` | o passo 2 do boot — report do spooler (payload editável no textarea para testar a saneação do Rails) |
| `GET /impressora/config` | puxa a config da impressora (colunas/codepage/corte/poll) e passa a usar isso no render do papel |
| `GET /impressora/jobs` | o poll — puxa os jobs pendentes com o payload em base64; cada job vira uma folha de papel renderizada |
| `POST /impressora/jobs/:id/ack` | confirma a impressão (botão ACK por folha, "ACK pendentes" ou ACK automático, como o agente) |
| `POST /impressora/logs` | os logs do debug_mode — payload `{"entries":[...]}` editável para testar a saneação |

O poll automático replica o agente: intervalo da config (default 3s) e o
heartbeat — o mesmo `last_seen_at` que o card usa para "conectada / offline
desde HH:MM" (régua de 1 min). Se o poll anunciar `config_version` nova, o
tester refaz o `GET /config` (decisão 15), como o agente faz.

A **ordem do boot do agente** (decisão 17) está no contrato do tester:
token → `POST /impressora/printers` (passo 2) → só então `GET /config`
(passo 3) → loop poll→ack (passo 4).

## Alvos

- **Local** — Rails de dev (`http://localhost:3000`); URL editável.
- **Localíssimo** — produção (`https://localissimo.com.br`); URL editável.
- **Demo** — servidor fake embutido no HTML: mesmo contrato do Rails (espelha
  o `PrintDevicesController`), com 3 pedidos de exemplo (2 entregas + 1
  retirada = 5 comandas) e saneação com os MESMOS tetos (50 impressoras × 20
  chaves; 200 entries × 2000 chars). Não precisa de token nem de rede.

### Sobre os requests reais (importante)

Os endpoints do S4 vivem na branch `feat/comandas-termicas` do Rails. Uma
página local (`file://`) só conversa com o Rails se ele responder o preflight
OPTIONS / liberar CORS nos endpoints do device (o token no header
`Authorization` dispara preflight) — o middleware `PrintAgentCors` já vem na
branch, liberado só em `/impressora`. "Falha de rede" no log = é isso. O
agente Go ignora CORS por natureza.

## Cadastrar o device de um negócio (pegar o token)

O token pertence a um `print_device` (mesmo padrão do entregador — ADR-0001).
No modo agent do quadro de pedidos, o card Impressora cria/reusa o device
"Agente no balcão" e mostra o token com copiar. Ou, num shell:

```bash
cd ~/mv/projects/localissimo

bin/rails runner '
  negocio = Business.order(:id).first   # ou find_by!(name: "Nome do negócio")

  device = PrintDevice.create!(business: negocio, name: "Tester HTML")
  Printer.find_or_create_by!(business: negocio) do |p|
    p.assign_attributes(name: "Cozinha – Tomate MDK-080",
                        host: "192.168.0.50", port: 9100)
  end
  puts device.access_token
'
```

Cole o token no campo do topo (fica no `localStorage` por servidor).
"ACK pendentes"/"ACK automático" marcam `printed` no servidor de verdade —
os jobs somem do próximo poll.

## Interpretador ESC/POS embutido

O papel é renderizado por um interpretador do subconjunto que o builder do
S2 (`app/values/esc_pos.rb`) emite: `ESC @`, `ESC !`/`GS !` (duplo
altura/largura), `ESC a` (alinhamento), `ESC E`/`ESC -` (negrito/sublinhado),
`ESC d`/`ESC J` (alimentação), `ESC t` (codepage) e `GS V` (**corte** —
marcado com ✂ no papel). Codepages UTF-8/CP860/CP850 decodificam por tabela
aproximada. Cada folha tem o detalhe "bytes ESC/POS" com o hex dump, para
conferir golden bytes do S2.

No modo Demo dá para trocar colunas (32/48), codepage e corte: "restaurar
comandas demo" re-enfileira os pedidos **com o payload congelado nas novas
configurações** — igual ao Rails (decisão 5: payload pronto na enfileiração).
