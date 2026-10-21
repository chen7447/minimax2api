@echo off
setlocal EnableExtensions
cd /d "%~dp0"

rem ============================================================
rem  Minimax2api.bat - build / run helper for MiniMax2API
rem
rem  Usage:
rem    Minimax2api.bat              build frontend + backend (default)
rem    Minimax2api.bat build        same as above
rem    Minimax2api.bat fe           build the frontend only
rem    Minimax2api.bat be           build the backend only
rem    Minimax2api.bat pack         cross-compile linux/amd64 and bundle deploy tar.gz
rem    Minimax2api.bat run [key]    build, then start the server
rem                                 (optional initial admin key)
rem    Minimax2api.bat test         run "go test ./..." and "go vet ./..."
rem    Minimax2api.bat clean        delete minimax2api.exe
rem    Minimax2api.bat help         show this help
rem
rem  Notes:
rem    - The Go toolchain is located via PATH first, then common
rem      install locations (C:\Program Files\Go\bin etc.).
rem    - The server runs with the repo root as working dir, so the
rem      defaults apply: data dir .\data, static .\frontend\dist,
rem      listen addr 127.0.0.1:8080 (override via MINIMAX2API_ADDR).
rem    - Without MINIMAX2API_ADMIN_KEY the first start prints a
rem      generated admin key to this console.
rem ============================================================

set "EXE=minimax2api.exe"

set "ACTION=%~1"
if not defined ACTION set "ACTION=build"

if /i "%ACTION%"=="build" goto build
if /i "%ACTION%"=="fe" goto fe
if /i "%ACTION%"=="be" goto be
if /i "%ACTION%"=="pack" goto pack
if /i "%ACTION%"=="run" goto run
if /i "%ACTION%"=="capture" goto capture
if /i "%ACTION%"=="login" goto login
if /i "%ACTION%"=="test" goto test
if /i "%ACTION%"=="clean" goto clean
if /i "%ACTION%"=="help" goto help
if /i "%ACTION%"=="/h" goto help
if /i "%ACTION%"=="-h" goto help
if /i "%ACTION%"=="--help" goto help

echo [minimax2api] unknown option: %ACTION%
call :help
exit /b 1

:help
echo Minimax2api.bat - build / run helper for MiniMax2API on Windows
echo.
echo   Minimax2api.bat              build frontend + backend (default)
echo   Minimax2api.bat build        same as above
echo   Minimax2api.bat fe           build the frontend only
echo   Minimax2api.bat be           build the backend only
echo   Minimax2api.bat pack         cross-compile linux/amd64 + bundle deploy tar.gz
echo   Minimax2api.bat run [key]    build, then start the server
echo   Minimax2api.bat capture      start the local credential-capture helper
echo   Minimax2api.bat login ...    register/login a fresh intl account into the pool
echo                                (node tools/login.mjs --help for flags)
echo   Minimax2api.bat test         go test + go vet in backend
echo   Minimax2api.bat clean        delete minimax2api.exe
echo   Minimax2api.bat help         show this help
echo.
echo   Admin console: http://127.0.0.1:8080   (single-key sign in)
echo   API endpoint:  http://127.0.0.1:8080/v1/chat/completions
exit /b 0

:build
call :do_frontend || exit /b 1
call :do_backend || exit /b 1
exit /b 0

:fe
call :do_frontend || exit /b 1
exit /b 0

:be
call :do_backend || exit /b 1
exit /b 0

:test
call :need_go || exit /b 1
pushd backend
echo [minimax2api] running go test ./...
go test ./... || (popd & exit /b 1)
echo [minimax2api] running go vet ./...
go vet ./... || (popd & exit /b 1)
popd
exit /b 0

:clean
if exist "%EXE%" (
    del /q "%EXE%"
    echo [minimax2api] removed %EXE%
) else (
    echo [minimax2api] nothing to clean
)
exit /b 0

:capture
rem The helper needs the console address and the admin key. Both come from
rem capture.local.json, which lives outside the repository next to the other
rem local secrets; extra arguments are passed straight through.
echo [minimax2api] starting the credential-capture helper ...
node "%~dp0tools\capture.mjs" %2 %3 %4 %5 %6 %7 %8 %9
exit /b %ERRORLEVEL%

:login
rem Reproducible intl registration/login pipeline (tools/login.mjs). Reads
rem capture.local.json for the console address and admin key; the mail API key
rem and account flags come straight from the operator.
echo [minimax2api] starting the login pipeline ...
node "%~dp0tools\login.mjs" %2 %3 %4 %5 %6 %7 %8 %9
exit /b %ERRORLEVEL%

:run
if not "%~2"=="" set "MINIMAX2API_ADMIN_KEY=%~2"
call :do_backend || exit /b 1
if exist "frontend\dist\index.html" (
    echo [minimax2api] frontend dist found - skipping its build. Use "fe" to rebuild.
) else (
    echo [minimax2api] frontend dist missing - building it ...
    call :do_frontend || exit /b 1
)
if not exist "data" mkdir "data"
echo [minimax2api] starting server ...
echo [minimax2api]   Admin console: http://127.0.0.1:8080  (single-key sign in)
echo [minimax2api]   API endpoint:  http://127.0.0.1:8080/v1/chat/completions
echo [minimax2api]   press Ctrl+C to stop
"%EXE%"
exit /b %errorlevel%

:pack
call :do_frontend || exit /b 1
call :need_go || exit /b 1
if exist "deploy\pack" rmdir /s /q "deploy\pack"
if not exist "deploy\pack" mkdir "deploy\pack"
echo [minimax2api] cross-compiling linux/amd64 ...
pushd backend
set "GOOS=linux"
set "GOARCH=amd64"
go build -trimpath -ldflags "-s -w" -o ..\deploy\pack\minimax2api .\cmd\minimax2api
if errorlevel 1 (
    popd
    echo [minimax2api] ERROR: linux/amd64 build failed.
    exit /b 1
)
popd
set "GOOS="
set "GOARCH="
copy /y "deploy\minimax2api.service" "deploy\pack\minimax2api.service" >nul || exit /b 1
copy /y "deploy\DEPLOY.md" "deploy\pack\DEPLOY.md" >nul || exit /b 1
robocopy "frontend\dist" "deploy\pack\dist" /E /NFL /NDL /NJH /NJS >nul
if errorlevel 8 (
    echo [minimax2api] ERROR: copying frontend dist failed.
    exit /b 1
)
pushd deploy\pack
tar -czf ..\minimax2api-deploy-linux-amd64.tar.gz DEPLOY.md minimax2api dist minimax2api.service
if errorlevel 1 (
    popd
    echo [minimax2api] ERROR: tar failed.
    exit /b 1
)
popd
echo [minimax2api] deploy package: %CD%\deploy\minimax2api-deploy-linux-amd64.tar.gz
exit /b 0

:do_frontend
where npm >nul 2>nul || call :find_npm
where npm >nul 2>nul || (
    echo [minimax2api] ERROR: npm not found. Install Node.js 20+ from https://nodejs.org/
    exit /b 1
)
pushd frontend
if not exist "node_modules" (
    echo [minimax2api] installing frontend dependencies ...
    call npm install || (popd & exit /b 1)
)
echo [minimax2api] building frontend ...
call npm run build || (popd & exit /b 1)
popd
if not exist "frontend\dist\index.html" (
    echo [minimax2api] ERROR: frontend\dist\index.html missing after build.
    exit /b 1
)
echo [minimax2api] frontend OK: frontend\dist
exit /b 0

:find_npm
for %%P in ("%ProgramFiles%\nodejs" "%ProgramFiles(x86)%\nodejs" "%USERPROFILE%\scoop\apps\nodejs\current" "%ProgramData%\chocolatey\bin") do (
    if exist "%%~P\npm.cmd" call :add_path "%%~P"
)
goto :eof

:do_backend
call :need_go || exit /b 1
pushd backend
echo [minimax2api] building backend ...
go build -trimpath -ldflags "-s -w" -o ..\%EXE% .\cmd\minimax2api
if errorlevel 1 (
    popd
    echo [minimax2api] ERROR: backend build failed.
    exit /b 1
)
popd
if not exist "%EXE%" (
    echo [minimax2api] ERROR: build reported success but %EXE% is missing.
    exit /b 1
)
echo [minimax2api] backend OK: %CD%\%EXE%
exit /b 0

rem ---- locate the Go toolchain: PATH first, then common install dirs ----
:need_go
where go >nul 2>nul && goto :eof
for %%P in ("%ProgramFiles%\Go\bin" "%ProgramFiles(x86)%\Go\bin" "%SystemDrive%\Go\bin" "%USERPROFILE%\scoop\apps\go\current\bin" "%ProgramData%\chocolatey\bin" "%ProgramData%\chocolatey\lib\golang\tools\go\bin") do (
    if exist "%%~P\go.exe" call :add_path "%%~P"
)
where go >nul 2>nul && goto :eof
echo [minimax2api] ERROR: Go toolchain not found on PATH or in common locations.
echo            Install Go 1.24 or newer:  winget install GoLang.Go
echo            or download from https://go.dev/dl/
exit /b 1

:add_path
set "PATH=%~1;%PATH%"
goto :eof
