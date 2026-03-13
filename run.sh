#!/bin/bash
# goclaudeclaw 自動更新 watchdog
# 每次重啟前都從 GitHub 拉取最新代碼並重新編譯
set -e

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$SCRIPT_DIR"

GOBIN=/data/go/go/bin/go
LOG=/tmp/goclaudeclaw.log

while true; do
    echo "[$(date '+%Y-%m-%d %H:%M:%S')] 拉取最新代碼..." | tee -a "$LOG"
    git pull origin main 2>&1 | tee -a "$LOG"

    echo "[$(date '+%Y-%m-%d %H:%M:%S')] 編譯中..." | tee -a "$LOG"
    if $GOBIN build -o goclaudeclaw.new ./cmd/goclaudeclaw/ 2>&1 | tee -a "$LOG"; then
        mv goclaudeclaw.new goclaudeclaw
        echo "[$(date '+%Y-%m-%d %H:%M:%S')] 編譯成功" | tee -a "$LOG"
    else
        echo "[$(date '+%Y-%m-%d %H:%M:%S')] 編譯失敗，使用舊版本" | tee -a "$LOG"
    fi

    echo "[$(date '+%Y-%m-%d %H:%M:%S')] 啟動 goclaudeclaw..." | tee -a "$LOG"
    ./goclaudeclaw >> "$LOG" 2>&1
    EXIT_CODE=$?
    echo "[$(date '+%Y-%m-%d %H:%M:%S')] goclaudeclaw 退出 (code=$EXIT_CODE)，3 秒後重啟..." | tee -a "$LOG"
    sleep 3
done
