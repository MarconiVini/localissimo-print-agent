#!/usr/bin/env bash
# uninstall.sh — remove o serviço printer-agent do systemd. Os arquivos da
# pasta (token.txt, agent.log) são preservados — só a unit sai (o agente
# para de subir no boot). Uso: sudo ./uninstall.sh
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
printf '%s  Vamos começar a desinstalação...%s\n' "$C_ACA" "$C_0"
sleep 1

passo "[1/3] Verificando permissões (root)..."
if [ "$(id -u)" -ne 0 ]; then
  falhou
  echo
  printf '%s  [erro] rode como root: sudo ./uninstall.sh%s\n' "$C_ERR" "$C_0"
  exit 1
fi
ok
sleep 1

passo "[2/3] Localizando o serviço..."
if [ ! -e "/etc/systemd/system/$SVC.service" ]; then
  echo "      · o serviço $SVC não está instalado"
  echo
  echo "  Nada a fazer — este computador não tinha o agente registrado."
  exit 0
fi
echo "      OK - instalado; parando e desativando..."
systemctl disable --now "$SVC"
ok
sleep 1

passo "[3/3] Removendo o registro do serviço..."
rm -f "/etc/systemd/system/$SVC.service"
systemctl daemon-reload
systemctl reset-failed "$SVC" 2>/dev/null || true
ok

echo
echo "  =============================================================="
echo
printf '      %s[OK]     D E S I N S T A L A D O%s\n' "$C_OK" "$C_0"
echo
echo "      O serviço \"Localissimo Printer Agent\" foi removido"
echo "      do systemd: ele não sobe mais no boot."
echo
echo "      · os arquivos desta pasta foram preservados"
echo "        (token.txt, agent.log) — apague a pasta se quiser"
echo "        remover tudo"
echo "      · para reinstalar depois, rode o install.sh desta"
echo "        pasta (sudo ./install.sh)"
echo
echo "  =============================================================="
