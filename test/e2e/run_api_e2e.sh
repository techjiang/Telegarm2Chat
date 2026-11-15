#!/bin/bash
# API 端到端测试：起一个干净实例，跑完自动清理。
# 用法: ./run_api_e2e.sh [端口]
set -u
PORT="${1:-18080}"
DIR="$(cd "$(dirname "$0")" && pwd)"
ROOT="$(cd "$DIR/../.." && pwd)"
DATA=$(mktemp -d)
LOG=$(mktemp)

cd "$ROOT"
go build -o "$DATA/t2c" ./cmd/t2c || exit 1

T2C_DATA_DIR="$DATA" T2C_ADDR=":$PORT" "$DATA/t2c" > "$LOG" 2>&1 &
SRV=$!
trap 'kill $SRV 2>/dev/null; rm -rf "$DATA" "$LOG"' EXIT

for _ in $(seq 1 60); do
  curl -sf -m 1 "localhost:$PORT/healthz" > /dev/null && break
  sleep 0.25
done

T2C_PORT="$PORT" python3 "$DIR/api_e2e.py"
