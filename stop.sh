#!/usr/bin/env bash
# 停止 claude-proxy(按 pidfile)。
set -euo pipefail
PID="$HOME/.claude-proxy/proxy.pid"
if [[ -f "$PID" ]] && kill -0 "$(cat "$PID")" 2>/dev/null; then
  kill "$(cat "$PID")"
  echo "已停止 pid=$(cat "$PID")"
  rm -f "$PID"
else
  echo "未在运行(无有效 pidfile)"
fi
echo "提示: 用 'claude-proxy run -- ...' 启动 claude 会自动拉起代理,无需手动改 settings.json"
