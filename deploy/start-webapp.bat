@echo off
chcp 65001 >nul
title WorkBuddy2API @VERSION@
cd /d "%~dp0"

rem =====================================================================
rem  WorkBuddy2API startup script (shipped inside the release package)
rem
rem  Two rules this file follows, both learned the hard way:
rem   1) `chcp 65001` must come before ANY Chinese text, otherwise cmd.exe
rem      renders the banners as mojibake (default console codepage is 936).
rem   2) The listen address defaults to 127.0.0.1, NOT 0.0.0.0. The admin
rem      console shares this port; binding 0.0.0.0 exposes it to everyone
rem      on the LAN/WAN. Change ADDR below only when you really need it.
rem =====================================================================

set "ADDR=127.0.0.1:8780"

if not exist "webapp.exe" (
  echo [错误] 找不到 webapp.exe —— 请把本脚本与 webapp.exe 放在同一个文件夹。
  pause
  exit /b 1
)
if not exist "data" mkdir "data"

rem 管理密钥优先级：环境变量 WB2API_ADMINKEY > data\admin.key > 首次启动自动生成。
rem 这里只负责把已存在的密钥喂给程序；不写死任何值（写死等于随包外发一把钥匙）。
if not defined WB2API_ADMINKEY if exist "data\admin.key" set /p WB2API_ADMINKEY=<"data\admin.key"

echo.
echo   WorkBuddy2API 中转站 @VERSION@
echo   ==================================================
echo   管理后台 : http://%ADDR%/admin.html
echo   用户首页 : http://%ADDR%/
echo   健康检查 : http://%ADDR%/healthz
echo   --------------------------------------------------
if defined WB2API_ADMINKEY (
  echo   管理密钥 : 已读取（data\admin.key），启动日志里脱敏显示
) else (
  echo   管理密钥 : 首次启动自动生成，保存在 data\admin.key
  echo              启动后请看下面日志里那行「管理密钥 ****」
)
echo   数据库   : .\data\webapp.db（备份就拷这个文件 + -wal/-shm）
echo   停止服务 : 在本窗口按 Ctrl+C
echo   --------------------------------------------------
echo   只监听本机（127.0.0.1）：别的电脑访问不到，这是安全默认值。
echo   要让局域网/公网访问：右键编辑本文件，把 ADDR 改成 0.0.0.0:8780，
echo   并务必先改掉管理密钥（设置环境变量 WB2API_ADMINKEY）。
echo   ==================================================
echo.

if defined WB2API_ADMINKEY (
  webapp.exe -addr %ADDR% -db .\data\webapp.db -key %WB2API_ADMINKEY%
) else (
  webapp.exe -addr %ADDR% -db .\data\webapp.db
)

if errorlevel 1 (
  echo.
  echo [服务已退出] 如果上面有报错，常见原因：
  echo   - 8780 端口被占用：换一个端口，或先关掉占用它的程序
  echo   - data 目录不可写：不要放在 C:\Program Files 下，放到桌面或 D 盘
  echo.
  pause
)
