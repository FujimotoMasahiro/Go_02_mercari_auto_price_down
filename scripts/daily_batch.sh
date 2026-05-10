#!/bin/bash
# メルカリ 毎朝バッチ: 自動値下げ → 売れ筋リサーチ → macOS通知

set -uo pipefail

ROOT_DIR="/Users/fujimotomasahiro/Documents/Golang/Go_02_mercari_auto_price_down"
GOBIN="/usr/local/go/bin/go"
cd "$ROOT_DIR"

LOG_DIR="$ROOT_DIR/data/log"
mkdir -p "$LOG_DIR"
LOG_FILE="$LOG_DIR/batch_$(date +%Y%m%d).log"

log() {
    echo "[$(date '+%Y-%m-%d %H:%M:%S')] $*" | tee -a "$LOG_FILE"
}

notify() {
    local title="$1"
    local message="$2"
    osascript -e "display notification \"$message\" with title \"$title\" sound name \"Glass\"" 2>/dev/null || true
}

elapsed() {
    local start="$1"
    local end
    end=$(date +%s)
    echo $(( end - start ))
}

log "=============================="
log "バッチ処理 開始"
log "=============================="

# ── バイナリビルド ──────────────────────────────────────────────────────────
log "バイナリをビルド中..."
if ! "$GOBIN" build -o bin/discount ./cmd/discount/ 2>>"$LOG_FILE"; then
    log "ERROR: discount バイナリのビルドに失敗しました"
    notify "メルカリ自動化" "⚠ バイナリビルドに失敗しました"
    exit 1
fi
if ! "$GOBIN" build -o bin/estimate-shipping ./cmd/estimate-shipping/ 2>>"$LOG_FILE"; then
    log "ERROR: estimate-shipping バイナリのビルドに失敗しました"
    notify "メルカリ自動化" "⚠ バイナリビルドに失敗しました"
    exit 1
fi
if ! "$GOBIN" build -o bin/research ./cmd/research/ 2>>"$LOG_FILE"; then
    log "ERROR: research バイナリのビルドに失敗しました"
    notify "メルカリ自動化" "⚠ バイナリビルドに失敗しました"
    exit 1
fi
log "バイナリビルド完了"

# ── 自動値下げ ──────────────────────────────────────────────────────────────
log "--- 自動値下げ 開始 ---"
notify "メルカリ自動化" "自動値下げを開始します"
DISCOUNT_START=$(date +%s)

DISCOUNT_OK=true
if "$ROOT_DIR/bin/discount" >> "$LOG_FILE" 2>&1; then
    DISCOUNT_SEC=$(elapsed "$DISCOUNT_START")
    log "--- 自動値下げ 完了 (${DISCOUNT_SEC}秒) ---"
    notify "メルカリ自動化" "自動値下げが完了しました（${DISCOUNT_SEC}秒）"
else
    DISCOUNT_SEC=$(elapsed "$DISCOUNT_START")
    log "--- 自動値下げ 失敗 (${DISCOUNT_SEC}秒) ---"
    notify "メルカリ自動化" "⚠ 自動値下げに失敗しました"
    DISCOUNT_OK=false
fi

sleep 3

# ── 送料自動推定 ────────────────────────────────────────────────────────────
log "--- 送料自動推定 開始 ---"
SHIPPING_START=$(date +%s)

SHIPPING_OK=true
if "$ROOT_DIR/bin/estimate-shipping" >> "$LOG_FILE" 2>&1; then
    SHIPPING_SEC=$(elapsed "$SHIPPING_START")
    log "--- 送料自動推定 完了 (${SHIPPING_SEC}秒) ---"
else
    SHIPPING_SEC=$(elapsed "$SHIPPING_START")
    log "--- 送料自動推定 失敗 (${SHIPPING_SEC}秒) ---"
    SHIPPING_OK=false
fi

sleep 3

# ── 売れ筋リサーチ ──────────────────────────────────────────────────────────
log "--- 売れ筋リサーチ 開始 ---"

# config.json からリサーチ条件を読み込む
RESEARCH_ARGS=()
if [ -f "$ROOT_DIR/config.json" ]; then
    PAGES=$(python3 -c "import json; d=json.load(open('config.json')); print(d.get('researchConditions',{}).get('pages',5))" 2>/dev/null || echo "5")
    CATEGORY_ID=$(python3 -c "import json; d=json.load(open('config.json')); print(d.get('researchConditions',{}).get('categoryId',''))" 2>/dev/null || echo "")
    KEYWORD=$(python3 -c "import json; d=json.load(open('config.json')); print(d.get('researchConditions',{}).get('keyword',''))" 2>/dev/null || echo "")
    PRICE_MIN=$(python3 -c "import json; d=json.load(open('config.json')); print(d.get('researchConditions',{}).get('priceMin',0))" 2>/dev/null || echo "0")
    PRICE_MAX=$(python3 -c "import json; d=json.load(open('config.json')); print(d.get('researchConditions',{}).get('priceMax',0))" 2>/dev/null || echo "0")
    CONDITIONS=$(python3 -c "import json; d=json.load(open('config.json')); print(','.join(str(x) for x in d.get('researchConditions',{}).get('conditions',[])))" 2>/dev/null || echo "")
    SHIPPING=$(python3 -c "import json; d=json.load(open('config.json')); print(d.get('researchConditions',{}).get('shippingPayer',''))" 2>/dev/null || echo "")

    RESEARCH_ARGS+=("-pages" "${PAGES:-5}")
    [ -n "$CATEGORY_ID" ]             && RESEARCH_ARGS+=("-category-id" "$CATEGORY_ID")
    [ -n "$KEYWORD" ]                  && RESEARCH_ARGS+=("-keyword"     "$KEYWORD")
    [ "${PRICE_MIN:-0}" -gt 0 ]        && RESEARCH_ARGS+=("-price-min"   "$PRICE_MIN")
    [ "${PRICE_MAX:-0}" -gt 0 ]        && RESEARCH_ARGS+=("-price-max"   "$PRICE_MAX")
    [ -n "$CONDITIONS" ]               && RESEARCH_ARGS+=("-conditions"  "$CONDITIONS")
    [ -n "$SHIPPING" ]                 && RESEARCH_ARGS+=("-shipping-payer" "$SHIPPING")
else
    RESEARCH_ARGS+=("-pages" "5")
fi

log "リサーチ引数: ${RESEARCH_ARGS[*]:-なし}"
notify "メルカリ自動化" "売れ筋リサーチを開始します"
RESEARCH_START=$(date +%s)

RESEARCH_OK=true
if "$ROOT_DIR/bin/research" "${RESEARCH_ARGS[@]}" >> "$LOG_FILE" 2>&1; then
    RESEARCH_SEC=$(elapsed "$RESEARCH_START")
    log "--- 売れ筋リサーチ 完了 (${RESEARCH_SEC}秒) ---"
    notify "メルカリ自動化" "売れ筋リサーチが完了しました（${RESEARCH_SEC}秒）"
else
    RESEARCH_SEC=$(elapsed "$RESEARCH_START")
    log "--- 売れ筋リサーチ 失敗 (${RESEARCH_SEC}秒) ---"
    notify "メルカリ自動化" "⚠ 売れ筋リサーチに失敗しました"
    RESEARCH_OK=false
fi

# ── 最終サマリー通知 ────────────────────────────────────────────────────────
log "=============================="
log "バッチ処理 完了"
log "=============================="

if $DISCOUNT_OK && $SHIPPING_OK && $RESEARCH_OK; then
    notify "メルカリ自動化" "本日のバッチ処理がすべて完了しました ✓"
else
    FAILED=""
    $DISCOUNT_OK  || FAILED+="値下げ"
    $SHIPPING_OK  || { [ -n "$FAILED" ] && FAILED+="・"; FAILED+="送料推定"; }
    $RESEARCH_OK  || { [ -n "$FAILED" ] && FAILED+="・"; FAILED+="リサーチ"; }
    notify "メルカリ自動化" "バッチ完了（失敗あり: ${FAILED}）"
fi
