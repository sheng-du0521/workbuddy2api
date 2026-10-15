#!/usr/bin/env bash
# dev.sh — 源码模式下的启停 helper（对应 docker compose 的 up/down/restart）
# 用法: ./dev.sh {start|stop|restart|status|logs|key}
set -uo pipefail
cd "$(dirname "$0")"

# v0.31.4 起源码模式的默认产品是 webapp（网站 + 管理后台 + 网关一体）；
# 旧网关（cmd/server）仍可用 `go run ./cmd/server -config config.json` 单独起，
# 按计划在合并第二步删除。
BIN=./webapp
LOG=./server.log
PIDFILE=./.server.pid
ADDR=127.0.0.1:8780
DB=./data/webapp.db

alive() { [[ -f $PIDFILE ]] && kill -0 "$(cat $PIDFILE)" 2>/dev/null; }

case "${1:-status}" in
  start)
    if alive; then echo "已在运行 (PID $(cat $PIDFILE))"; exit 0; fi
    # 二进制解析：缺失或构建输入（*.go 与 go.mod/go.sum）比它新时重编。过期的
    # webapp 影响的是全部转发流量（而非某个只读工具），比过期 CLI 更值得拦住。
    if [[ ! -x $BIN ]] || find . \( -name '*.go' -o -name 'go.mod' -o -name 'go.sum' \) -newer "$BIN" -print -quit | grep -q .; then
        if ! command -v go >/dev/null 2>&1; then
            echo "需要 go 构建 $BIN（源码模式需本机 Go；容器部署用 docker compose）" >&2
            exit 1
        fi
        echo "build webapp ..."
        # 本脚本没有 set -e（其余包装脚本都有），构建失败会继续往下走到 nohup，
        # 把陈旧的 webapp 拉起来并报「已启动」——正是本 PR 要消灭的形态，故显式判失败。
        mkdir -p ./data
        CGO_ENABLED=0 go build -o "$BIN" ./cmd/webapp || {
            echo "构建 $BIN 失败，未启动（避免沿用旧二进制）" >&2
            exit 1
        }
    fi
    # 不传 -key：管理密钥走 WB2API_ADMINKEY > data/admin.key > 首次随机生成并落盘。
    nohup "$BIN" -addr "$ADDR" -db "$DB" > "$LOG" 2>&1 &
    echo $! > "$PIDFILE"
    sleep 2
    if alive; then echo "已启动 (PID $(cat $PIDFILE))"; else echo "启动失败，见 $LOG"; tail -20 "$LOG"; exit 1; fi
    ;;
  stop)
    if alive; then kill "$(cat $PIDFILE)" && echo "已停止"; else echo "未在运行"; fi
    rm -f "$PIDFILE"
    ;;
  restart)
    "$0" stop; sleep 1; "$0" start
    ;;
  status)
    if alive; then
      KEY=$(cat ./data/admin.key 2>/dev/null || true)
      echo "运行中 (PID $(cat $PIDFILE))"
      curl -s "http://$ADDR/healthz" -w "\n" 2>/dev/null
      echo "账号数: $(curl -s "http://$ADDR/status" -H "Authorization: Bearer $KEY" 2>/dev/null | python3 -c 'import json,sys;print(len(json.load(sys.stdin).get("accounts",[])))' 2>/dev/null || echo '?')"
    else
      echo "未运行"
    fi
    ;;
  logs)    tail -f "$LOG" ;;
  key)     cat ./data/admin.key 2>/dev/null || echo "（还没有 data/admin.key：首次启动才会生成）" ;;
  *)       echo "用法: $0 {start|stop|restart|status|logs|key}"; exit 1 ;;
esac
