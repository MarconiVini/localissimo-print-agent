#!/usr/bin/env bash
# build.sh — gera as releases do printer-agent (epic §2: binário único em Go,
# estático CGO_ENABLED=0, cross-compilado para os 5 alvos). A única
# dependência externa é golang.org/x/sys (canônica do time Go), confinada ao
# harness de serviço do Windows (internal/winsvc) — não entra em
# linux/darwin.
#
# Uso:
#   ./build.sh 0.2.0     → dist/v0.2.0/ — pacote completo da versão
#   ./build.sh           → build de dev (0.0.0-dev; o auto-update ignora —
#                          versão suja não casa X.Y.Z, epic §7.4.1)
#
# Pacote por versão (cada nova versão é uma cópia completa; versões antigas
# ficam lado a lado e NUNCA são apagadas):
#
#   dist/v<versão>/
#   ├── SHA256SUMS.txt    ← todos os binários (fonte do catálogo Rails — é o
#   │                        que o form "create_agent_release" do card
#   │                        Comandas pede: version + download_url + sha256)
#   ├── LEIA-ME.txt       ← instalação/configuração/diagnose p/ a loja
#   ├── windows/  printer-agent-windows-amd64.exe
#   │             install.bat · uninstall.bat · token.txt (vazio)
#   ├── linux/    printer-agent-linux-amd64 · printer-agent-linux-arm64
#   │             install.sh · uninstall.sh · token.txt (vazio)
#   └── darwin/   printer-agent-darwin-{arm64,amd64} · token.txt (vazio;
#                 sem instalador por enquanto)
#
# Sem config.json no pacote: a URL de produção é default compilado do agente
# (internal/agent.DefaultServer = https://localissimo.com.br); -server e
# config.json seguem existindo como override dev/tester. Os instaladores
# registram a pasta onde estão (portável): copiou a pasta pro balcão, rodou
# install, pronto — upgrade de versão = rodar o install da pasta nova.
set -euo pipefail

VERSION="${1:-0.0.0-dev}"
PKG="dist/v$VERSION"

if ! echo "$VERSION" | grep -Eq '^[0-9]+\.[0-9]+\.[0-9]+$'; then
  echo "erro: versão '$VERSION' não casa X.Y.Z (o catálogo do Rails exige semver sem prefixo)" >&2
  exit 1
fi

cd "$(dirname "$0")"
if [ ! -f packaging/install.bat ]; then
  echo "erro: packaging/ não encontrado (os instaladores são copiados de lá)" >&2
  exit 1
fi

mkdir -p "$PKG/windows" "$PKG/linux" "$PKG/darwin"

# Alvos da tabela do epic §2 (Windows do balcão é o primário) → pasta do pacote.
TARGETS=(
  "windows windows-amd64 printer-agent-windows-amd64.exe"
  "linux linux-amd64 printer-agent-linux-amd64"
  "linux linux-arm64 printer-agent-linux-arm64"
  "darwin darwin-arm64 printer-agent-darwin-arm64"
  "darwin darwin-amd64 printer-agent-darwin-amd64"
)

HOST_OS="$(go env GOOS)"
HOST_ARCH="$(go env GOARCH)"

for entry in "${TARGETS[@]}"; do
  read -r osdir platform out <<< "$entry"
  goos="${platform%%-*}"
  goarch="${platform##*-}"
  echo "→ $osdir/$out (GOOS=$goos GOARCH=$goarch)"
  CGO_ENABLED=0 GOOS="$goos" GOARCH="$goarch" \
    go build -trimpath -ldflags "-s -w -X localissimo-print-agent/internal/agent.Version=$VERSION" \
    -o "$PKG/$osdir/$out" .
done

# token.txt vazio: ausente/vazio → o agente instrui no log e espera (epic §3
# passo 1 — o arquivo pode chegar depois; trocar token = editar o arquivo).
# Nunca shipar token real dentro do pacote: o token do device é gerado por
# loja no painel e colado no local.
for osdir in windows linux darwin; do
  : > "$PKG/$osdir/token.txt"
done

# Instaladores + LEIA-ME vivem no repo (packaging/) — o build só os copia.
cp packaging/install.bat packaging/uninstall.bat "$PKG/windows/"
cp packaging/install.sh packaging/uninstall.sh "$PKG/linux/"
cp packaging/LEIA-ME.txt "$PKG/"
chmod +x "$PKG"/linux/printer-agent-* "$PKG"/linux/install.sh "$PKG"/linux/uninstall.sh

# Validação do epic §7.2: rodar o binário alvo com -version e conferir a
# versão registrada — release mal registrada (ldflags esquecido) vira ciclo
# eterno de update no device. Roda nativamente quando o alvo == host; nos
# outros alvos a validação fica para o device/CI com emulador.
for entry in "${TARGETS[@]}"; do
  read -r osdir platform out <<< "$entry"
  goos="${platform%%-*}"
  goarch="${platform##*-}"
  exe="$PKG/$osdir/$out"
  if [ "$goos" = "$HOST_OS" ] && [ "$goarch" = "$HOST_ARCH" ]; then
    got="$("$exe" -version)"
    if [ "$got" != "$VERSION" ]; then
      echo "erro: $out respondeu -version '$got' (esperado $VERSION)" >&2
      exit 1
    fi
    echo "✓ $out -version = $got (validado nativamente)"
  else
    echo "• $out: -version não validável neste host ($HOST_OS/$HOST_ARCH) — validar no alvo/CI"
  fi
done

# SHA256SUMS.txt: a fonte dos hashes para o catálogo printer_agent_releases.
cd "$PKG"
sha256sum windows/printer-agent-* linux/printer-agent-* darwin/printer-agent-* > SHA256SUMS.txt
cd - >/dev/null

echo
echo "=== Pacote em $PKG/ (versão $VERSION) ==="
find "$PKG" -type f | sort
echo
cat "$PKG/SHA256SUMS.txt"
