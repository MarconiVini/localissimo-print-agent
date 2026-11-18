@echo off
rem uninstall.bat - remove o servico printer-agent do Windows. Os arquivos
rem da pasta (token.txt, agent.log) sao preservados - so o registro no SCM
rem sai (o agente para de subir no boot).
rem
rem A janela mostra o passo a passo e NUNCA fecha sozinha no fim: sucesso ou
rem erro, sempre termina no resumo + "Pressione qualquer tecla".
setlocal EnableExtensions
set "EXITCODE=0"
set "SVC=printer-agent"

title Localissimo Printer Agent - Desinstalacao
cls
echo.
echo   ==============================================================
echo     L O C A L I S S I M O   -   Printer Agent (comandas 80mm)
echo   ==============================================================
echo.
echo    Vamos comecar a desinstalacao...
echo.
timeout /t 1 /nobreak >nul

echo    [1/3] Verificando permissoes de administrador...
net session >nul 2>&1
if errorlevel 1 goto :noadmin
echo          OK
timeout /t 1 /nobreak >nul

echo    [2/3] Localizando o servico...
sc query %SVC% >nul 2>&1
if errorlevel 1 goto :absent
echo          OK - instalado; parando...
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
echo          OK
timeout /t 1 /nobreak >nul

echo    [3/3] Removendo o registro do servico...
sc delete %SVC% >nul 2>&1
echo          OK

echo.
echo   ==============================================================
echo.
echo          [OK]     D E S I N S T A L A D O
echo.
echo          O servico "Localissimo Printer Agent" foi removido
echo          do Windows: ele nao sobe mais no boot.
echo.
echo          * os arquivos desta pasta foram preservados
echo            (token.txt, agent.log) - apague a pasta se quiser
echo            remover tudo
echo          * para reinstalar depois, rode o install.bat desta
echo            pasta como administrador
echo.
echo   ==============================================================
goto :fim

:absent
echo          - o servico %SVC% nao esta instalado
echo.
echo    Nada a fazer - este computador nao tinha o agente registrado.
goto :fim

:noadmin
echo          FALHOU
echo.
echo    [ERRO] Esta janela nao tem permissoes de administrador.
echo           Feche-a, clique com o direito no uninstall.bat e escolha
echo           "Executar como administrador".
set "EXITCODE=1"
goto :fim

:fim
echo.
echo    Pressione qualquer tecla para encerrar...
pause >nul
exit /b %EXITCODE%
