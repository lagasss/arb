#!/usr/bin/env bash
# watch_node_sync.sh — polls the remote Arbitrum Nitro node until it's healthy,
# then switches config.yaml from Alchemy to ws://localhost:8548 and restarts the bot.
#
# Runs in background: nohup bash scripts/watch_node_sync.sh &

REMOTE="seb@209.172.45.63"
CONFIG="/home/arbitrator/go/arb-bot/config.yaml"
BOT_DIR="/home/arbitrator/go/arb-bot"
LOG="/home/arbitrator/go/arb-bot/scripts/watch_node_sync.log"
POLL_INTERVAL=120
MAX_LAG_BLOCKS=50
ARB_HEAD_URL="https://arb1.arbitrum.io/rpc"

log() { echo "[$(date '+%Y-%m-%d %H:%M:%S')] $*" | tee -a "$LOG"; }

get_arb_head() {
    curl -sf "$ARB_HEAD_URL" \
        -X POST -H 'Content-Type: application/json' \
        -d '{"jsonrpc":"2.0","method":"eth_blockNumber","params":[],"id":1}' \
    | python3 -c 'import sys,json; print(int(json.load(sys.stdin)["result"],16))' 2>/dev/null \
    || echo 0
}

# Run a multi-step health check on the remote node.
# Prints a one-line status and exits 0 only when node is fully synced.
check_node() {
    ssh -o ConnectTimeout=15 -o BatchMode=yes -o StrictHostKeyChecking=no "$REMOTE" '
        # Step 1: container running?
        if ! docker ps --format "{{.Names}}" 2>/dev/null | grep -q "^arbitrum-nitro$"; then
            echo "FAIL:container_not_running"
            exit 1
        fi

        # Step 2: get recent logs (last 30 lines)
        RECENT=$(docker logs arbitrum-nitro --tail 30 2>&1)

        # Step 3: still in snapshot download?
        if echo "$RECENT" | grep -qF "Downloading database part"; then
            PROGRESS=$(echo "$RECENT" | grep -oE "[0-9]+ / [0-9]+" | tail -1)
            echo "FAIL:downloading_snapshot:${PROGRESS}"
            exit 2
        fi
        if echo "$RECENT" | grep -qF "Downloading initial database"; then
            echo "FAIL:init_download_starting"
            exit 2
        fi

        # Step 4: RPC HTTP responding?
        BLOCK_HEX=$(curl -sf --max-time 5 http://localhost:8547 \
            -X POST -H "Content-Type: application/json" \
            -d "{\"jsonrpc\":\"2.0\",\"method\":\"eth_blockNumber\",\"params\":[],\"id\":1}" \
            2>/dev/null | python3 -c "import sys,json; print(json.load(sys.stdin)[\"result\"])" 2>/dev/null)
        if [ -z "$BLOCK_HEX" ]; then
            echo "FAIL:rpc_not_responding"
            exit 3
        fi
        LOCAL_BLOCK=$(python3 -c "print(int(\"$BLOCK_HEX\", 16))" 2>/dev/null || echo 0)

        # Step 5: still syncing?
        SYNCING=$(curl -sf --max-time 5 http://localhost:8547 \
            -X POST -H "Content-Type: application/json" \
            -d "{\"jsonrpc\":\"2.0\",\"method\":\"eth_syncing\",\"params\":[],\"id\":1}" \
            2>/dev/null | python3 -c "import sys,json; r=json.load(sys.stdin)[\"result\"]; print(\"false\" if r is False else \"syncing\")" 2>/dev/null)
        if [ "$SYNCING" != "false" ]; then
            echo "FAIL:still_syncing:block=$LOCAL_BLOCK"
            exit 4
        fi

        # Step 6: not stuck on repeated inbox errors?
        STUCK=$(echo "$RECENT" | grep -c "error reading inbox" || true)
        if [ "$STUCK" -gt 5 ]; then
            echo "FAIL:stuck_inbox:errors=$STUCK:block=$LOCAL_BLOCK"
            exit 5
        fi

        # Step 7: WebSocket port open?
        if ! nc -z -w3 localhost 8548 2>/dev/null; then
            echo "FAIL:ws_port_closed:block=$LOCAL_BLOCK"
            exit 6
        fi

        echo "OK:block=$LOCAL_BLOCK:inbox_errors=$STUCK"
        exit 0
    ' 2>&1
}

switch_config() {
    log "Switching config.yaml to local node..."
    cp "$CONFIG" "${CONFIG}.bak.$(date +%s)"
    sed -i 's|arbitrum_rpc:.*|arbitrum_rpc: "ws://localhost:8548"|' "$CONFIG"
    log "config.yaml → ws://localhost:8548"
}

restart_bot() {
    log "Rebuilding bot..."
    export PATH=$PATH:/usr/local/go/bin
    cd "$BOT_DIR"
    if ! go build -o arb-bot ./cmd/bot/ >> "$LOG" 2>&1; then
        log "ERROR: build failed — keeping Alchemy config"
        sed -i 's|arbitrum_rpc:.*|arbitrum_rpc: "wss://arb-mainnet.g.alchemy.com/v2/E0ASSpGbenMwsLFIfcr1-"|' "$CONFIG"
        return 1
    fi
    pkill -f './arb-bot' 2>/dev/null || true
    sleep 2
    nohup ./arb-bot >> "$BOT_DIR/bot.log" 2>&1 &
    log "Bot restarted on local RPC (PID $!)"
}

# ── Main loop ─────────────────────────────────────────────────────────────────

log "=== Node sync watcher started (PID $$) ==="
log "Remote: $REMOTE  Poll: ${POLL_INTERVAL}s  Max lag: ${MAX_LAG_BLOCKS} blocks"

while true; do
    HEAD=$(get_arb_head)
    log "Arbitrum head: $HEAD — checking node..."

    OUTPUT=$(check_node)
    EXIT_CODE=$?
    log "Node: $OUTPUT (exit=$EXIT_CODE)"

    if [ "$EXIT_CODE" -eq 0 ]; then
        LOCAL=$(echo "$OUTPUT" | grep -oE 'block=[0-9]+' | head -1 | cut -d= -f2)
        LOCAL=${LOCAL:-0}
        LAG=$(( HEAD - LOCAL ))
        log "Synced! local=$LOCAL head=$HEAD lag=$LAG blocks"

        if [ "$LAG" -le "$MAX_LAG_BLOCKS" ]; then
            log "✓ Node healthy — switching over"
            switch_config
            restart_bot
            log "=== Switchover complete. Watcher exiting. ==="
            exit 0
        else
            log "Waiting for node to catch up (lag=$LAG > $MAX_LAG_BLOCKS)"
        fi
    fi

    log "Next check in ${POLL_INTERVAL}s..."
    sleep "$POLL_INTERVAL"
done
