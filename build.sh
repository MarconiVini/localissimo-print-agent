#!/usr/bin/env bash
# build.sh — gera as releases do printer-agent (epic §2: binário único em Go,
# só stdlib, estático CGO_ENABLED=0, cross-compilado para os 5 alvos).
#
# Uso:
#   ./build.sh 0.1.0     → dist/printer-agent-<os>-<arch>[.exe] + SHA256SUMS.txt
#   ./build.sh           → build de dev (0.0.0-dev: o auto-update ignora —
#                          versão suja não casa X.Y.Z, epic §7.4.1)
#
# Os hashes saem no dist/SHA256SUMS.txt — é exatamente o que o form
# "create_agent_release" do card Comandas pede (version + download_url + sha256).
set -euo pipefail

VERSION="${1:-0.0.0-dev}"
DIST="dist"

if ! echo "$VERSION" | grep -Eq '^[0-9]+\.[0-9]+\.[0-9]+$'; then
  echo "erro: versão '$VERSION' não casa X.Y.Z (o catálogo do Rails exige semver sem prefixo)" >&2
  exit 1
fi

cd "$(dirname "$0")"
mkdir -p "$DIST"

# Alvos da tabela do epic §2 (Windows do balcão é o primário).
TARGETS=(
  "windows-amd64 printer-agent-windows-amd64.exe"
  "linux-amd64 printer-agent-linux-amd64"
  "linux-arm64 printer-agent-linux-arm64"
  "darwin-arm64 printer-agent-darwin-arm64"
  "darwin-amd64 printer-agent-darwin-amd64"
)

HOST_OS="$(go env GOOS)"
HOST_ARCH="$(go env GOARCH)"

for entry in "${TARGETS[@]}"; do
  read -r platform out <<< "$entry"
  goos="${platform%%-*}"
  goarch="${platform##*-}"
  echo "→ $out (GOOS=$goos GOARCH=$goarch)"
  CGO_ENABLED=0 GOOS="$goos" GOARCH="$goarch" \
    go build -trimpath -ldflags "-s -w -X localissimo-print-agent/internal/agent.Version=$VERSION" \
    -o "$DIST/$out" .
done

# Validação do epic §7.2: rodar o binário alvo com -version e conferir a
# versão registrada — release mal registrada (ldflags esquecido) vira ciclo
# eterno de update no device. Roda nativamente quando o alvo == host; nos
# outros alvos a validação fica para o device/CI com emulador.
for entry in "${TARGETS[@]}"; do
  read -r platform out <<< "$entry"
  goos="${platform%%-*}"
  goarch="${platform##*-}"
  if [ "$goos" = "$HOST_OS" ] && [ "$goarch" = "$HOST_ARCH" ]; then
    got="$("$DIST/$out" -version)"
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
cd "$DIST"
sha256sum printer-agent-* > SHA256SUMS.txt
cd ..

echo
echo "=== Releases em $DIST/ (versão $VERSION) ==="
cat "$DIST/SHA256SUMS.txt"
