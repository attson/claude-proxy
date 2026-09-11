#!/usr/bin/env bash
# 启动 claude-proxy(nohup 常驻)。重复启动前先检查端口占用。
set -euo pipefail

DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
GO="${GO_BIN:-$HOME/sdk/go1.23.12/bin/go}"
PORT="${CLAUDE_PROXY_PORT:-36240}"
LOG="$HOME/.claude-proxy/proxy.log"
PID="$HOME/.claude-proxy/proxy.pid"

mkdir -p "$HOME/.claude-proxy/samples"
chmod 700 "$HOME/.claude-proxy" "$HOME/.claude-proxy/samples"

# 端口占用检查
if ss -ltn 2>/dev/null | grep -q ":${PORT} "; then
  echo "端口 ${PORT} 已被占用,先 ./stop.sh 或换端口(CLAUDE_PROXY_PORT)" >&2
  exit 1
fi

echo "编译..."
( cd "$DIR" && "$GO" build -o claude-proxy . )

echo "启动 (nohup),日志 -> $LOG"
nohup "$DIR/claude-proxy" > "$LOG" 2>&1 &
echo $! > "$PID"
sleep 1
if kill -0 "$(cat "$PID")" 2>/dev/null; then
  echo "已启动 pid=$(cat "$PID")  监听 127.0.0.1:${PORT}"
  echo "接入: 把 ~/.claude/settings.json 的 env.ANTHROPIC_BASE_URL 改为 http://127.0.0.1:${PORT} 并重启 Claude Code"
else
  echo "启动失败,看日志: $LOG" >&2
  tail -n 20 "$LOG" >&2 || true
  exit 1
fi
