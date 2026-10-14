#!/usr/bin/env bash
# make-release.sh —— 生成**可安全外发**的发布包（v0.24.0）
#
# 为什么需要它：`dist/` 既是分发目录又是运行目录，里面混着
#   · data/secret.key   主密钥（能解密全部上游凭证）
#   · data/webapp.db    生产数据库（真实账号、口令哈希、会话）
#   · data/admin.key    管理密钥
# 直接打包 dist/ 就等于把密钥和数据库一起发出去。本脚本只挑「程序 + 启动脚本 + 文档」，
# 并在打包前做一轮断言，发现敏感物就中止。
#
# 用法：scripts/make-release.sh [输出目录]   默认 ./release
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
OUT="${1:-$ROOT/release}"
SRC="$ROOT/dist"

# 允许放行的文件（白名单）：只含程序、启动/探测脚本、对外文档。
FILES=(
  webapp.exe
  start-webapp.bat
  start-tunnel.bat
  stop-tunnel.bat
  tunnel-url.bat
  check-status.bat
  config.json
  Web应用使用说明.md
  免费代理测试报告.md
)

rm -rf "$OUT"
mkdir -p "$OUT"

echo "==> 收集发布文件"
for f in "${FILES[@]}"; do
  if [[ -f "$SRC/$f" ]]; then
    cp -p "$SRC/$f" "$OUT/$f"
    echo "    + $f"
  else
    echo "    - 跳过（不存在）：$f"
  fi
done

echo "==> 断言：发布包内不得含敏感物"
failed=0

# 1) 目录级：绝不允许 data/ 或任何密钥文件
for bad in data secret.key admin.key '*.db' '*.log'; do
  if compgen -G "$OUT/$bad" > /dev/null 2>&1; then
    echo "    ✗ 发现敏感物：$bad" >&2
    failed=1
  fi
done

# 2) 内容级：明文密钥特征串
if grep -rIl -e 'zoH7f8x5zi3H9u9KQ' -e 'wb-local-key-please-change' "$OUT" 2>/dev/null | grep -q .; then
  echo "    ✗ 文件中含已知明文密钥特征" >&2
  failed=1
fi

# 3) config.json 的 api_key 必须为空
if [[ -f "$OUT/config.json" ]]; then
  if ! python - "$OUT/config.json" <<'PY'
import json,sys
d=json.load(open(sys.argv[1],encoding='utf-8'))
sys.exit(0 if not str(d.get('api_key','')).strip() else 1)
PY
  then
    echo "    ✗ config.json 的 api_key 非空（不得随包分发明文密钥）" >&2
    failed=1
  fi
fi

if [[ "$failed" != 0 ]]; then
  echo "==> 打包中止：请先清理上述敏感物" >&2
  exit 1
fi

echo "==> 通过。发布包：$OUT"
ls -la "$OUT"
echo
echo "提醒：管理员密钥由程序首次启动时生成到运行目录的 data/admin.key，不在本包内。"
