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
echo "回退提醒: 记得把 ~/.claude/settings.json 的 ANTHROPIC_BASE_URL 改回 https://api.anthropic.com/ 并重启 Claude Code"
