@echo off
rem install.bat - registra o printer-agent como servico nativo do Windows.
rem Uso: clique com o direito neste arquivo > "Executar como administrador",
rem a partir da pasta do pacote (a pasta onde o .bat esta e a que fica
rem registrada no servico - o pacote e portavel).
rem Idempotente: se o servico ja existe, ele e recriado apontando para ESTA
rem pasta (e assim que se faz upgrade de versao / rollback).
rem Supervisor e o proprio Windows (SCM): sobe no boot sem login; se o
rem processo cair, religa em ~10s (sc failure abaixo).
rem
rem A janela mostra o passo a passo e NUNCA fecha sozinha no fim: sucesso ou
rem erro, sempre termina no resumo + "Pressione qualquer tecla" (rodando via
rem "Executar como administrador" o console fecha quando o .bat sai - o
rem pause e o que garante o feedback visivel).
setlocal EnableExtensions
set "EXITCODE=0"
set "SVC=printer-agent"
set "EXE=%~dp0printer-agent-windows-amd64.exe"
set "VER="

title Localissimo Printer Agent - Instalacao
cls
echo.
echo   ==============================================================
echo     L O C A L I S S I M O   -   Printer Agent (comandas 80mm)
echo   ==============================================================
echo.
echo    Vamos comecar a instalacao...
echo.
timeout /t 1 /nobreak >nul

echo    [1/5] Verificando permissoes de administrador...
net session >nul 2>&1
if errorlevel 1 goto :noadmin
echo          OK
timeout /t 1 /nobreak >nul

echo    [2/5] Verificando o executavel do agente...
if not exist "%EXE%" goto :noexe
for /f "delims=" %%v in ('"%EXE%" -version 2^>nul') do set "VER=%%v"
if defined VER (echo          OK - versao %VER%) else echo          OK
timeout /t 1 /nobreak >nul

echo    [3/5] Preparando o servico...
sc query %SVC% >nul 2>&1
if errorlevel 1 goto :create
echo          - ja existia: parando para recriar apontando para esta pasta
sc stop %SVC% >nul 2>&1
set /a TRY=0
:waitstop
timeout /t 2 /nobreak >nul
set /a TRY+=1
sc query %SVC% | find "STOPPED" >nul
if not errorlevel 1 goto :stopped
if %TRY% LSS 15 goto :waitstop
echo          - [aviso] nao parou em 30s - o delete pode ficar pendente de reboot
:stopped
sc delete %SVC% >nul 2>&1
:create
rem aspas internas escapadas no binPath sao obrigatorias para o sc aceitar
rem caminho com espacos + argumentos (aqui: -spawnless).
sc create %SVC% binPath= "\"%EXE%\" -spawnless" start= auto DisplayName= "Localissimo Printer Agent" >nul
if errorlevel 1 goto :failcreate
echo          OK
timeout /t 1 /nobreak >nul

echo    [4/5] Configurando descricao e ressurreicao automatica...
sc description %SVC% "Agente de impressao termica do Localissimo (comandas 80mm)" >nul
rem Ressurreicao: crash ou exit != 0 religa em 10s (10s, 10s, 30s depois);
rem failureflag=1 estende as failure actions ao encerramento "limpo com
rem erro" - que e exatamente o fim de ciclo pos-update (troca de versao).
sc failure %SVC% reset= 86400 actions= restart/10000/restart/10000/restart/30000 >nul
sc failureflag %SVC% 1 >nul
echo          OK
timeout /t 1 /nobreak >nul

echo    [5/5] Iniciando servico...
sc start %SVC% >nul
if errorlevel 1 goto :failstart
echo          OK

echo.
echo   ==============================================================
echo.
echo          [OK]   I N S T A L A D O   C O M   S U C E S S O
echo.
echo          Localissimo Printer Agent %VER%
echo          Servico "%SVC%" registrado e iniciado
echo.
echo          * sobe no boot, sem login; se cair, religa em ~10s
echo          * cole o token do device no token.txt desta pasta
echo            (deteccao em ate 30s, sem reiniciar nada)
echo          * diagnose: sc query %SVC%  ou  services.msc
echo            ou o agent.log desta pasta
echo.
echo   ==============================================================
goto :fim

:noadmin
echo          FALHOU
echo.
echo    [ERRO] Esta janela nao tem permissoes de administrador.
echo           Feche-a, clique com o direito no install.bat e escolha
echo           "Executar como administrador".
set "EXITCODE=1"
goto :fim

:noexe
echo          FALHOU
echo.
echo    [ERRO] Nao achei o executavel do agente ao lado deste .bat:
echo           %EXE%
echo           Rode a partir da pasta do pacote (windows\).
set "EXITCODE=1"
goto :fim

:failcreate
echo          FALHOU
echo.
echo    [ERRO] sc create falhou - confira com: sc query %SVC%
set "EXITCODE=1"
goto :fim

:failstart
echo          FALHOU
echo.
echo    [ERRO] O servico foi criado mas nao iniciou. Confira:
echo           sc query %SVC%   e o agent.log desta pasta
set "EXITCODE=1"
goto :fim

:fim
echo.
echo    Pressione qualquer tecla para encerrar...
pause >nul
exit /b %EXITCODE%
