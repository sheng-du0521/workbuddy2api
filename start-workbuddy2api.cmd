@echo off
setlocal EnableDelayedExpansion
cd /d "%~dp0"

if not exist "webapp.exe" (
  echo Missing webapp.exe. Build it with: go build -o webapp.exe ./cmd/webapp
  exit /b 1
)
rem v0.31.4 起本脚本启动 webapp（网站 + 管理后台 + 网关一体，8780）。
rem 旧网关（wb2api.exe / config.json / 7863）按计划在合并第二步删除。

set "WB2API_ROOT=%CD%"
set "WB2API_EXE=%CD%\webapp.exe"
set "WB2API_PID_FILE=%CD%\webapp.pid"
if exist "%WB2API_PID_FILE%" (
  set /p WB2API_PID=<"%WB2API_PID_FILE%"
  powershell -NoProfile -Command "try { $p=Get-Process -Id ([int]$env:WB2API_PID) -ErrorAction Stop; if ([IO.Path]::GetFullPath($p.Path) -eq [IO.Path]::GetFullPath($env:WB2API_EXE)) { exit 0 } } catch {}; exit 1"
  if not errorlevel 1 (
    echo WorkBuddy2API is already running. PID=!WB2API_PID!
    exit /b 0
  )
  del /q "%WB2API_PID_FILE%" >nul 2>&1
)

if not exist "data" mkdir "data"
powershell -NoProfile -Command "$p=Start-Process -FilePath $env:WB2API_EXE -ArgumentList '-addr','127.0.0.1:8780','-db','.\data\webapp.db' -WorkingDirectory $env:WB2API_ROOT -WindowStyle Hidden -RedirectStandardOutput (Join-Path $env:WB2API_ROOT 'data\webapp.out.log') -RedirectStandardError (Join-Path $env:WB2API_ROOT 'data\webapp.err.log') -PassThru; [IO.File]::WriteAllText($env:WB2API_PID_FILE, [string]$p.Id)"
if errorlevel 1 exit /b 1

set /p WB2API_PID=<"%WB2API_PID_FILE%"
echo WorkBuddy2API started. PID=%WB2API_PID%
endlocal
