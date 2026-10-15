@echo off
setlocal EnableDelayedExpansion
chcp 65001 >nul

where go >nul 2>nul
if errorlevel 1 (
  echo [X] go not found in PATH. Install Go 1.22.5+ first.
  exit /b 1
)

for /f "tokens=3" %%v in ('go version') do echo [i] go version: %%v

set "ROOT=%~dp0"
set "OUT=%ROOT%dist"
if not exist "%OUT%" mkdir "%OUT%"

set CGO_ENABLED=0
set "LDFLAGS=-s -w"

echo.
echo === building workbuddy2api binaries ===

rem webapp = 主产品（Web 应用中转站，8780）。v0.24.0 补入：此前它**不在任何
rem 脚本化构建路径里**，是个手工产物 —— 于是"改了源码忘了重建 exe"极易发生。
rem v0.31.5 起旧网关（cmd/server → wb2api.exe）已随合并删除，不再构建。
call :build webapp webapp.exe
call :build login login.exe
call :build signin signin_bin.exe
call :build credit credit.exe
call :build acct acct.exe
call :build trial trial.exe
call :build activity activity.exe
call :build stats stats.exe

echo.
echo === done ===
dir /b "%OUT%"
exit /b 0

:build
echo --- cmd\%~1
pushd "%ROOT%"
go build -trimpath -ldflags="%LDFLAGS%" -o "%OUT%\%~2" .\cmd\%~1
if errorlevel 1 (
  echo [X] failed: %~1
) else (
  echo [OK] %~2
)
popd
exit /b 0
