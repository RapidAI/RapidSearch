@echo off
setlocal EnableExtensions
cd /d "%~dp0"

if not defined SEARCH_PROXY_HOST set SEARCH_PROXY_HOST=hub.maclaw.top

if not exist "%~dp0deploy-proxy-remote.sh" (
  echo error: deploy-proxy-remote.sh missing next to this script
  exit /b 1
)

where go >nul 2>&1
if errorlevel 1 (
  echo error: go not found on PATH
  exit /b 1
)
where ssh >nul 2>&1
if errorlevel 1 (
  echo error: ssh not found on PATH
  exit /b 1
)
where tar >nul 2>&1
if errorlevel 1 (
  echo error: tar not found on PATH (Windows 11 includes tar.exe)
  exit /b 1
)

echo building linux/amd64 search-proxy (must be ELF, not .exe)...
set GOOS=linux
set GOARCH=amd64
set CGO_ENABLED=0
go build -trimpath -ldflags="-s -w" -o search-proxy ./cmd/proxy
if errorlevel 1 exit /b 1

if exist search-proxy.exe (
  echo error: go produced search-proxy.exe; GOOS=linux was not applied
  exit /b 1
)
if not exist search-proxy (
  echo error: search-proxy binary missing after build
  exit /b 1
)

powershell -NoProfile -Command "$b=[IO.File]::ReadAllBytes((Resolve-Path 'search-proxy'))[0..3]; if ($b[0] -ne 127 -or $b[1] -ne 69 -or $b[2] -ne 76 -or $b[3] -ne 70) { Write-Host 'error: search-proxy is not a Linux ELF'; exit 1 }"
if errorlevel 1 exit /b 1

echo deploying to root@%SEARCH_PROXY_HOST% (ssh will prompt for root password once)...
REM One ssh: stream linux binary + deploy-proxy-remote.sh. type is not binary-safe, so tar is used.
tar -cf - search-proxy deploy-proxy-remote.sh | ssh -o StrictHostKeyChecking=accept-new root@%SEARCH_PROXY_HOST% "tar -xf - -C /tmp && mv -f /tmp/search-proxy /tmp/search-proxy.new && bash /tmp/deploy-proxy-remote.sh"
