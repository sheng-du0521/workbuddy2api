@echo off
chcp 65001 >nul
setlocal EnableDelayedExpansion
title WorkBuddy2API 一键部署
cd /d "%~dp0"

rem =====================================================================
rem  一键部署（Windows）：下载官方发行包 -> 解压 -> 启动
rem
rem  给谁用：不想装 Go、不想看文档、只想把它跑起来的人。
rem  做了什么：只做三件事，且每一步失败都会明确告诉你下一步怎么办。
rem
rem  为什么要这么多"失败处理"：这条链上有四个外部依赖（网络、GitHub API、
rem  下载地址、解压工具），任何一环失败，如果不解释，用户看到的就是一个
rem  闪一下就关掉的窗口 —— 那比报错还糟。
rem =====================================================================

set "REPO=sheng-du0521/workbuddy2api"
set "TARGET=%~dp0wb2api"

echo.
echo   WorkBuddy2API 一键部署
echo   ==================================================
echo   目标目录: %TARGET%
echo.

where curl >nul 2>&1
if errorlevel 1 (
  echo [错误] 找不到 curl。Windows 10 1803 之后的系统自带它。
  echo        请手动到 https://github.com/%REPO%/releases 下载 zip 解压后
  echo        双击 start-webapp.bat。
  pause
  exit /b 1
)

echo [1/4] 查询最新版本...
set "TAG="
for /f "usebackq delims=" %%i in (`powershell -NoProfile -ExecutionPolicy Bypass -Command "try { (Invoke-RestMethod -TimeoutSec 15 'https://api.github.com/repos/%REPO%/releases/latest').tag_name } catch { '' }"`) do set "TAG=%%i"
if "%TAG%"=="" (
  echo.
  echo [失败] 拿不到版本号。常见原因：网络访问不了 GitHub。
  echo        办法一：挂代理后重试（本窗口设置 set HTTPS_PROXY=http://127.0.0.1:7890）
  echo        办法二：手动下载 —— https://github.com/%REPO%/releases
  echo                 选 wb2api-*-windows-amd64.zip，解压后双击 start-webapp.bat
  pause
  exit /b 1
)
set "VER=%TAG:v=%"
echo       最新版本 %TAG%

set "URL=https://github.com/%REPO%/releases/download/%TAG%/wb2api-%VER%-windows-amd64.zip"
if not exist "%TARGET%" mkdir "%TARGET%"

echo [2/4] 下载安装包...
echo       %URL%
curl -L --fail --retry 2 --connect-timeout 20 -o "%TARGET%\pkg.zip" "%URL%"
if errorlevel 1 (
  echo.
  echo [失败] 下载没成功。可能是网络中断，或该版本还没有 windows 包。
  echo        手动下载地址：https://github.com/%REPO%/releases
  pause
  exit /b 1
)

echo [3/4] 解压...
tar -xf "%TARGET%\pkg.zip" -C "%TARGET%"
if errorlevel 1 (
  echo [失败] 解压失败。请手动解压 %TARGET%\pkg.zip 后双击其中的 start-webapp.bat。
  pause
  exit /b 1
)
del /q "%TARGET%\pkg.zip" >nul 2>&1

echo [4/4] 启动...
echo.
echo   浏览器稍后请打开: http://127.0.0.1:8780/admin.html
echo   管理密钥会显示在启动日志里（也保存在 wb2api\data\admin.key）。
echo.
start "" /d "%TARGET%" "%TARGET%\start-webapp.bat"
echo 已在新的窗口启动。本窗口可以关闭。
timeout /t 5 >nul
exit /b 0
