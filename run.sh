#!/bin/bash
# claudeclaw 自動更新 watchdog
#
# auto_update=true（預設）：啟動時立即跑當前版本，同時後台 git pull + rebuild。
#   更新完存成 claudeclaw.new，下次重啟才換入，不影響當次啟動。
# auto_update=false：只重啟，不更新。

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$SCRIPT_DIR"

GOBIN=/data/go/go/bin/go
LOG=/tmp/claudeclaw.log
CONFIG="$SCRIPT_DIR/config.json"
UPDATE_PID=""

get_auto_update() {
    python3 -c "
import json
try:
    d = json.load(open('$CONFIG'))
    print(str(d.get('auto_update', True)).lower())
except:
    print('true')
"
}

while true; do
    # 如果上次後台編譯完成，換入新版本
    if [ -f claudeclaw.new ]; then
        mv claudeclaw.new claudeclaw
        echo "[$(date '+%Y-%m-%d %H:%M:%S')] 已切換至新版本" | tee -a "$LOG"
    fi

    echo "[$(date '+%Y-%m-%d %H:%M:%S')] 啟動 claudeclaw..." | tee -a "$LOG"

    # 後台異步更新（在 claudeclaw 運行期間進行，不阻塞啟動）
    AUTO=$(get_auto_update)
    if [ "$AUTO" = "true" ]; then
        (
            echo "[$(date '+%Y-%m-%d %H:%M:%S')] [bg] 拉取最新代碼..." >> "$LOG"
            git pull origin main >> "$LOG" 2>&1
            echo "[$(date '+%Y-%m-%d %H:%M:%S')] [bg] 編譯中..." >> "$LOG"
            VERSION=$(git describe --tags --always 2>/dev/null || git rev-parse --short HEAD 2>/dev/null || echo "dev")
            LDFLAGS="-X github.com/lustan3216/claudeclaw/internal/buildinfo.Version=$VERSION"
            if $GOBIN build -ldflags "$LDFLAGS" -o claudeclaw.new ./cmd/claudeclaw/ >> "$LOG" 2>&1; then
                echo "[$(date '+%Y-%m-%d %H:%M:%S')] [bg] 新版本已就緒，下次重啟生效" | tee -a "$LOG"
            else
                rm -f claudeclaw.new
                echo "[$(date '+%Y-%m-%d %H:%M:%S')] [bg] 編譯失敗，保留當前版本" | tee -a "$LOG"
            fi
        ) &
        UPDATE_PID=$!
    fi

    # 運行主程序
    ./claudeclaw >> "$LOG" 2>&1
    EXIT_CODE=$?
    echo "[$(date '+%Y-%m-%d %H:%M:%S')] claudeclaw 退出 (code=$EXIT_CODE)，3 秒後重啟..." | tee -a "$LOG"

    # 等待後台更新完成（如果還在跑）
    if [ -n "$UPDATE_PID" ]; then
        wait "$UPDATE_PID" 2>/dev/null || true
        UPDATE_PID=""
    fi

    sleep 3
done
