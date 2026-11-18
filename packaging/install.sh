#!/usr/bin/env bash
# install.sh — registra o printer-agent como serviço systemd. Uso:
#   sudo ./install.sh    (a partir da pasta do pacote; a pasta onde o script
#                         está é a que fica registrada — o pacote é portável)
# Idempotente: se o serviço já existe, a unit é reescrita apontando para
# ESTA pasta (e assim que se faz upgrade de versão / rollback).
# Supervisor é o systemd: sobe no boot sem login (WantedBy=multi-user),
# religa em ~10s se o processo cair (Restart=on-failure + RestartSec).
# A saída é guiada: banner, passo a passo com pausas curtas e resumo final.
set -euo pipefail

SVC=printer-agent

# Cores só quando a saída é um terminal (pipado/log fica limpo).
if [ -t 1 ]; then
  C_OK=$'\033[32m'; C_ERR=$'\033[31m'; C_STEP=$'\033[1m'; C_ACA=$'\033[36m'; C_0=$'\033[0m'
else
  C_OK=''; C_ERR=''; C_STEP=''; C_ACA=''; C_0=''
fi

passo() { printf '\n%s  %s%s\n' "$C_STEP" "$1" "$C_0"; }
ok()    { printf '      %sOK%s\n' "$C_OK" "$C_0"; }
falhou() { printf '      %sFALHOU%s\n' "$C_ERR" "$C_0"; }

echo
echo "  =============================================================="
echo "    L O C A L I S S I M O   ·   Printer Agent (comandas 80mm)"
echo "  =============================================================="
echo
printf '%s  Vamos começar a instalação...%s\n' "$C_ACA" "$C_0"
sleep 1

passo "[1/5] Verificando permissões (root)..."
if [ "$(id -u)" -ne 0 ]; then
  falhou
  echo
  printf '%s  [erro] rode como root: sudo ./install.sh%s\n' "$C_ERR" "$C_0"
  exit 1
fi
ok
sleep 1

passo "[2/5] Verificando arquitetura e executável..."
case "$(uname -m)" in
  x86_64) ARCH=amd64 ;;
  aarch64 | arm64) ARCH=arm64 ;;
  *)
    falhou
    echo "  [erro] arquitetura não suportada: $(uname -m) (o pacote traz amd64/arm64)" >&2
    exit 1
    ;;
esac
DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
EXE="$DIR/printer-agent-linux-$ARCH"
if [ ! -x "$EXE" ]; then
  falhou
  echo "  [erro] binário não encontrado/executável: $EXE" >&2
  exit 1
fi
VER="$("$EXE" -version 2>/dev/null || echo '?')"
ok
sleep 1

passo "[3/5] Preparando o serviço systemd..."
systemctl stop "$SVC" 2>/dev/null || true

# -spawnless: pós-update não spawna filho — o agente encerra com falha e o
# Restart religa já no binário novo (mesmo contrato do SCM no Windows).
cat > "/etc/systemd/system/$SVC.service" <<EOF
[Unit]
Description=Localissimo Printer Agent (comandas térmicas 80mm)
After=network-online.target
Wants=network-online.target
StartLimitIntervalSec=0

[Service]
Type=simple
WorkingDirectory=$DIR
ExecStart=$EXE -spawnless
Restart=on-failure
RestartSec=10s

[Install]
WantedBy=multi-user.target
EOF
ok
sleep 1

passo "[4/5] Registrando e ativando na inicialização..."
if ! systemctl daemon-reload; then
  falhou
  echo "  [erro] systemctl daemon-reload falhou (veja: journalctl -xe)" >&2
  exit 1
fi
if ! systemctl enable "$SVC" >/dev/null 2>&1; then
  falhou
  echo "  [erro] systemctl enable $SVC falhou (veja: journalctl -xe)" >&2
  exit 1
fi
ok
sleep 1

passo "[5/5] Iniciando serviço..."
if ! systemctl start "$SVC"; then
  falhou
  echo "  [erro] o serviço foi registrado mas não iniciou. Confira:" >&2
  echo "         systemctl status $SVC · journalctl -u $SVC -n 50" >&2
  exit 1
fi
ok

echo
echo "  =============================================================="
echo
printf '      %s[OK]   I N S T A L A D O   C O M   S U C E S S O%s\n' "$C_OK" "$C_0"
echo
echo "      Localissimo Printer Agent ${VER}"
echo "      Serviço \"$SVC\" registrado e iniciado (systemd)"
echo
echo "      · sobe no boot, sem login; se cair, religa em ~10s"
echo "      · cole o token do device em $DIR/token.txt"
echo "        (detecção em até 30s, sem reiniciar nada)"
echo "      · diagnose: systemctl status $SVC ·"
echo "        journalctl -u $SVC -f · $DIR/agent.log"
echo
echo "  =============================================================="
