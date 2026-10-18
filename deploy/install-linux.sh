#!/usr/bin/env bash
# WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
# 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
# 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。
#
# install.sh —— Linux 一键部署（随发布包分发，v0.31.44）
#
# 用法（在解压出来的目录里）：
#     sudo ./install.sh                 # 装到 /opt/wb2api 并注册 systemd
#     sudo ./install.sh /srv/wb2api     # 换目录
#     ./install.sh --no-systemd         # 只铺文件，不装服务（容器/无 systemd）
#
# 设计约束（每条都对应一次踩坑）：
#   1. **幂等**：重复执行不覆盖 data/（密钥、数据库都在里面），只更新二进制。
#   2. **不猜地址**：默认 127.0.0.1:8780。要公网就自己改 -addr，脚本不替你开放。
#   3. **先备份**：替换二进制前把旧的存成 webapp.bak-<时间>，回滚只要一条 mv。
#   4. **失败要说清下一步**：端口占用、非 root、无 systemd 都给明确提示，
#      而不是丢一句 "failed"。
set -euo pipefail

DIR="${1:-/opt/wb2api}"
USE_SYSTEMD=1
for a in "$@"; do
  case "$a" in
    --no-systemd) USE_SYSTEMD=0 ;;
    -*) echo "未知参数：$a"; exit 2 ;;
    *)  DIR="$a" ;;
  esac
done

SRC="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
[ -f "$SRC/webapp" ] || { echo "错误：本目录没有 webapp，请在解压出来的发布目录里执行"; exit 1; }

if [ "$USE_SYSTEMD" = "1" ] && [ "$(id -u)" != "0" ]; then
  echo "错误：安装 systemd 服务需要 root。请用：sudo ./install.sh"
  echo "      只想铺文件不装服务：./install.sh --no-systemd"
  exit 1
fi

echo "==> 目标目录 $DIR"
mkdir -p "$DIR/data"

# 覆盖前备份（数据目录不动）。静默替换二进制是运维事故的常见起点。
if [ -f "$DIR/webapp" ]; then
  BAK="$DIR/webapp.bak-$(date +%Y%m%d-%H%M%S)"
  cp -p "$DIR/webapp" "$BAK"
  echo "    已备份旧版本 -> $BAK"
fi

install -m 0755 "$SRC/webapp" "$DIR/webapp"
for f in "快速开始.txt" "Web应用使用说明.md"; do
  [ -f "$SRC/$f" ] && cp -p "$SRC/$f" "$DIR/" || true
done

# 管理密钥：只读不造。程序首次启动会自己生成 data/admin.key。
if [ -f "$DIR/data/admin.key" ]; then
  echo "    管理密钥：沿用已有 $DIR/data/admin.key（不覆盖）"
else
  echo "    管理密钥：首次启动自动生成到 $DIR/data/admin.key"
fi

# 端口占用检查：这一步能省掉 90% 的「装完打不开」。
PORT="${WB2API_PORT:-8780}"
if command -v ss >/dev/null 2>&1 && ss -lnt 2>/dev/null | grep -q ":$PORT "; then
  echo "⚠️  端口 $PORT 已被占用。服务仍会安装，但启动会失败。"
  echo "    查占用：ss -lntp | grep :$PORT"
  echo "    换端口：改 /etc/systemd/system/webapp.service 里的 -addr"
fi

if [ "$USE_SYSTEMD" = "0" ] || ! command -v systemctl >/dev/null 2>&1; then
  echo
  echo "==> 未安装 systemd 服务。手动启动："
  echo "    cd $DIR && ./webapp -addr 127.0.0.1:$PORT -db ./data/webapp.db"
  echo "    （要后台常驻可用 nohup，或交给容器编排）"
  exit 0
fi

UNIT=/etc/systemd/system/webapp.service
cat > "$UNIT" <<EOF
[Unit]
Description=WorkBuddy2API webapp (OpenAI-compatible gateway + admin UI)
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
WorkingDirectory=$DIR
ExecStart=$DIR/webapp -addr 127.0.0.1:$PORT -db $DIR/data/webapp.db
Restart=on-failure
RestartSec=3
# 让 SQLite 的 WAL 与 -shm 稳定落盘，避免断电后要人工修复
LimitNOFILE=65535

[Install]
WantedBy=multi-user.target
EOF

systemctl daemon-reload
systemctl enable webapp >/dev/null 2>&1 || true
systemctl restart webapp
sleep 2

if systemctl is-active --quiet webapp; then
  echo "==> 服务已启动：$(systemctl is-active webapp)"
  if command -v curl >/dev/null 2>&1; then
    if curl -fsS --noproxy '*' "http://127.0.0.1:$PORT/healthz" >/dev/null 2>&1; then
      echo "    健康检查通过 http://127.0.0.1:$PORT/healthz"
    else
      echo "⚠️  健康检查未通过。看日志：journalctl -u webapp -n 50 --no-pager"
    fi
  fi
  echo
  echo "管理密钥（登录 /admin.html 用）："
  if [ -f "$DIR/data/admin.key" ]; then
    echo "    $(cat "$DIR/data/admin.key")"
  else
    echo "    尚未生成 —— 看 journalctl -u webapp -n 30 | grep 管理密钥"
  fi
  echo
  echo "常用命令："
  echo "    systemctl status webapp      查看状态"
  echo "    journalctl -u webapp -f      跟踪日志"
  echo "    systemctl restart webapp     重启"
  echo "    备份：cp $DIR/data/webapp.db* /somewhere/"
else
  echo "❌ 服务启动失败。看日志：journalctl -u webapp -n 50 --no-pager"
  exit 1
fi
