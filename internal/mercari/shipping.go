package mercari

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/chromedp/chromedp"
	"mercari-pricelower/internal/config"
	"mercari-pricelower/internal/costs"
	"mercari-pricelower/internal/logger"
)

// ShippingEstimate は送料自動推定の結果を保持します。
type ShippingEstimate struct {
	Method     string `json:"method"`
	Cost       int    `json:"cost"`
	Confidence string `json:"confidence"` // "high" | "medium" | "low" | "none"
	SampleSize int    `json:"sampleSize"`
}

var yearPattern = regexp.MustCompile(`\b(19|20)\d{2}\b`)

var stopWords = map[string]struct{}{
	"美品": {}, "新品": {}, "未使用": {}, "ジャンク": {}, "難あり": {},
	"傷あり": {}, "汚れあり": {}, "訳あり": {}, "動作確認済": {}, "動作品": {},
	"付き": {}, "セット": {}, "おまけ": {}, "ケース": {}, "カバー": {},
	"保証書": {}, "箱付き": {}, "説明書": {},
	"個": {}, "枚": {}, "本": {}, "冊": {}, "点": {}, "まとめ": {},
	"送料込み": {}, "送料無料": {}, "着払い": {},
}

// ensureLogger は appLogger が nil の場合に一時ロガーを初期化します。
func ensureLogger() {
	if appLogger == nil {
		l, err := logger.New()
		if err == nil {
			appLogger = l
		}
	}
}

// EstimateShipping は itemName と price を元に配送方法・送料を推定します。
// Chrome が未起動の場合は自動で起動します（/tmp/chrome-debug のCookieを引き継ぎます）。
func EstimateShipping(itemName string, price int) (ShippingEstimate, error) {
	ensureLogger()

	if !isChromeRunning() {
		launchChrome()
		// Chrome が DevTools ポートを開くまで待機
		if err := waitForPort("localhost:9222", 10*time.Second); err != nil {
			return ShippingEstimate{Confidence: "none"}, fmt.Errorf("Chrome起動タイムアウト: %w", err)
		}
	}

	tab, err := newTabContext()
	if err != nil {
		return ShippingEstimate{Confidence: "none"}, fmt.Errorf("タブ作成失敗: %w", err)
	}
	defer tab.Close()

	ctx, cancel := context.WithTimeout(tab.ctx, 90*time.Second)
	defer cancel()

	// メルカリTOPに遷移してCookieを読み込む（ログイン状態を引き継ぐ）
	if err := chromedp.Run(ctx, chromedp.Navigate("https://jp.mercari.com/")); err != nil {
		return ShippingEstimate{Confidence: "none"}, fmt.Errorf("メルカリTOP遷移失敗: %w", err)
	}
	time.Sleep(1 * time.Second)

	keywords := extractKeywords(itemName)
	if len(keywords) == 0 {
		return ShippingEstimate{Confidence: "none"}, nil
	}

	candidateIDs, err := searchSoldCandidates(ctx, keywords, price)
	if err != nil || len(candidateIDs) == 0 {
		return ShippingEstimate{Confidence: "none"}, nil
	}

	methods := make([]string, 0, len(candidateIDs))
	for _, id := range candidateIDs {
		m := scrapeShippingMethod(ctx, id)
		if m != "" && m != "未定" {
			methods = append(methods, m)
		}
		time.Sleep(400 * time.Millisecond)
	}

	if len(methods) == 0 {
		return ShippingEstimate{Confidence: "none", SampleSize: len(candidateIDs)}, nil
	}

	method, count := majorityMethod(methods)
	cost := estimateCost(method, price)

	confidence := "low"
	ratio := float64(count) / float64(len(methods))
	switch {
	case len(methods) >= 3 && ratio >= 0.7:
		confidence = "high"
	case len(methods) >= 2 && ratio >= 0.5:
		confidence = "medium"
	}

	return ShippingEstimate{
		Method:     method,
		Cost:       cost,
		Confidence: confidence,
		SampleSize: len(methods),
	}, nil
}

// extractKeywords は商品名から検索用キーワードを抽出します（最大4トークン）。
func extractKeywords(name string) []string {
	re := regexp.MustCompile(`[【】「」『』()（）\[\]／/\\|★☆◆◇■□●○▲▼]`)
	cleaned := re.ReplaceAllString(name, " ")
	cleaned = yearPattern.ReplaceAllString(cleaned, " ")

	var tokens []string
	for _, tok := range strings.Fields(cleaned) {
		if len([]rune(tok)) < 2 {
			continue
		}
		if _, skip := stopWords[tok]; skip {
			continue
		}
		tokens = append(tokens, tok)
	}
	if len(tokens) > 4 {
		tokens = tokens[:4]
	}
	return tokens
}

// searchSoldCandidates は売り切れ商品を検索してスコアリングし、上位候補IDを返します。
func searchSoldCandidates(ctx context.Context, keywords []string, refPrice int) ([]string, error) {
	query := strings.Join(keywords, " ")
	searchURL := "https://jp.mercari.com/search/?keyword=" + url.QueryEscape(query) + "&status=sold_out"

	// research.go の extractItemsJS と同じ堅牢なセレクタパターンを使用
	const extractJS = `(function() {
		const items = [];
		const seen = new Set();
		const links = Array.from(document.querySelectorAll('a[href*="/item/"]'));
		links.forEach(link => {
			const href = link.getAttribute('href') || '';
			const match = href.match(/\/item\/(m\w+)/);
			if (!match) return;
			const id = match[1];
			if (seen.has(id)) return;
			seen.add(id);
			let name = '';
			const labelEl = link.querySelector('[data-testid="item-label"]') ||
			                link.querySelector('[data-testid="item-name"]');
			if (labelEl) name = labelEl.textContent.trim();
			if (!name) {
				const al = link.getAttribute('aria-label') || '';
				if (al && !al.includes('円')) name = al.trim();
			}
			const imgEl = link.querySelector('img');
			if (!name && imgEl) {
				let alt = (imgEl.getAttribute('alt') || '').replace(/のサムネイル$/, '').trim();
				if (alt) name = alt;
			}
			if (!name) {
				const textEl = link.querySelector('p') || link.querySelector('span');
				if (textEl) name = textEl.textContent.trim();
			}
			const priceEl = link.querySelector('[data-testid="price"]') ||
			                link.querySelector('span[class*="price"]') ||
			                link.querySelector('[aria-label*="円"]');
			const priceText = priceEl ? priceEl.textContent.trim() : '';
			if (id) items.push({id, name, priceText});
		});
		return JSON.stringify(items.slice(0, 20));
	})()`

	navigateWithStatus(ctx, searchURL)
	time.Sleep(3 * time.Second)

	var resultJSON string
	if err := chromedp.Run(ctx, chromedp.Evaluate(extractJS, &resultJSON)); err != nil {
		return nil, err
	}

	type rawResult struct {
		ID        string `json:"id"`
		Name      string `json:"name"`
		PriceText string `json:"priceText"`
	}
	var rawResults []rawResult
	if err := json.Unmarshal([]byte(resultJSON), &rawResults); err != nil {
		return nil, fmt.Errorf("JSON parse error (raw=%q): %w", resultJSON, err)
	}
	if appLogger != nil {
		appLogger.Info("送料推定", "検索ヒット数", fmt.Sprintf("%d件 (query=%q)", len(rawResults), strings.Join(keywords, " ")))
	}

	type scored struct {
		id    string
		score int
	}
	var scoredList []scored
	for _, r := range rawResults {
		if r.ID == "" {
			continue
		}
		score := 0
		p := parsePriceText(r.PriceText)
		if p > 0 && refPrice > 0 {
			ratio := math.Abs(float64(p-refPrice)) / float64(refPrice)
			if ratio <= 0.3 {
				score += 2
			} else if ratio <= 0.6 {
				score += 1
			}
		}
		nameLower := strings.ToLower(r.Name)
		for _, kw := range keywords {
			if strings.Contains(nameLower, strings.ToLower(kw)) {
				score++
			}
		}
		if score >= 1 {
			scoredList = append(scoredList, scored{r.ID, score})
		}
	}

	// スコア降順ソート（バブルソート）
	for i := 0; i < len(scoredList)-1; i++ {
		for j := i + 1; j < len(scoredList); j++ {
			if scoredList[j].score > scoredList[i].score {
				scoredList[i], scoredList[j] = scoredList[j], scoredList[i]
			}
		}
	}

	maxN := 5
	if len(scoredList) < maxN {
		maxN = len(scoredList)
	}
	ids := make([]string, maxN)
	for i := 0; i < maxN; i++ {
		ids[i] = scoredList[i].id
	}
	return ids, nil
}

// scrapeShippingMethod は商品詳細ページから配送方法を取得します。
func scrapeShippingMethod(ctx context.Context, itemID string) string {
	const shippingJS = `(function() {
		const text = document.body ? (document.body.innerText || '') : '';
		const methods = [
			'ネコポス','宅急便コンパクト','らくらくメルカリ便',
			'ゆうパケット','ゆうゆうメルカリ便',
			'クロネコヤマト','ヤマト宅急便',
			'ゆうパック','定形外郵便','普通郵便',
			'着払い','未定'
		];
		for (const m of methods) {
			if (text.includes(m)) return m;
		}
		return '';
	})()`

	pageCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()

	if _, err := navigateWithStatus(pageCtx, "https://jp.mercari.com/item/"+itemID); err != nil {
		return ""
	}
	time.Sleep(600 * time.Millisecond)

	var method string
	chromedp.Run(pageCtx, chromedp.Evaluate(shippingJS, &method))
	return method
}

// majorityMethod は配送方法リストから最多票の方法とその件数を返します。
// 同票の場合は送料が安い方を選びます。
func majorityMethod(methods []string) (string, int) {
	counts := make(map[string]int)
	for _, m := range methods {
		counts[m]++
	}
	best, bestCount := "", 0
	for m, c := range counts {
		if c > bestCount || (c == bestCount && estimateCost(m, 1000) < estimateCost(best, 1000)) {
			best, bestCount = m, c
		}
	}
	return best, bestCount
}

// estimateCost は配送方法と価格から送料（円）を返します。
func estimateCost(method string, price int) int {
	switch {
	case strings.Contains(method, "ネコポス"):
		return 210
	case strings.Contains(method, "宅急便コンパクト"):
		return 450
	case strings.Contains(method, "らくらくメルカリ便"):
		if price <= 1500 {
			return 210
		}
		return 750
	case strings.Contains(method, "ゆうパケット"):
		return 230
	case strings.Contains(method, "ゆうゆうメルカリ便"):
		if price <= 1500 {
			return 230
		}
		return 770
	case strings.Contains(method, "クロネコ"), strings.Contains(method, "ヤマト"):
		return 750
	case strings.Contains(method, "ゆうパック"):
		return 770
	case strings.Contains(method, "定形外"), strings.Contains(method, "普通郵便"):
		if price <= 500 {
			return 120
		}
		return 200
	case strings.Contains(method, "着払い"), strings.Contains(method, "未定"), method == "":
		return 0
	default:
		return 600
	}
}

// EstimateAllMissing は最新CSVの出品一覧を読み込み、
// 送料が未設定（shippingCost == 0）の商品について自動推定を実行して costs.json に保存します。
// バッチ処理から呼び出すことを想定しています。
func EstimateAllMissing() error {
	ensureLogger()

	// ── 最新CSVから出品商品一覧を取得 ─────────────────────────────────────
	csvDir := config.DirCSV
	entries, err := os.ReadDir(csvDir)
	if err != nil {
		return fmt.Errorf("CSVディレクトリ読み込み失敗: %w", err)
	}
	var csvFiles []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".csv") {
			csvFiles = append(csvFiles, e.Name())
		}
	}
	if len(csvFiles) == 0 {
		if appLogger != nil {
			appLogger.Info("送料推定バッチ", "スキップ", "CSVファイルが見つかりません")
		}
		return nil
	}
	sort.Sort(sort.Reverse(sort.StringSlice(csvFiles)))

	f, err := os.Open(filepath.Join(csvDir, csvFiles[0]))
	if err != nil {
		return fmt.Errorf("CSV読み込み失敗: %w", err)
	}
	records, err := csv.NewReader(f).ReadAll()
	f.Close()
	if err != nil {
		return fmt.Errorf("CSV解析失敗: %w", err)
	}

	// id → {name, price}
	type listing struct {
		name  string
		price int
	}
	items := make(map[string]listing)
	for i, row := range records {
		if i == 0 || len(row) < 3 {
			continue
		}
		p := parsePriceText(row[2])
		items[row[0]] = listing{name: row[1], price: p}
	}

	// ── 送料未設定の商品を抽出 ────────────────────────────────────────────
	cb, _ := costs.Load()
	type target struct {
		id    string
		name  string
		price int
	}
	var targets []target
	for id, it := range items {
		c := cb[id]
		if c.ShippingCost == 0 {
			targets = append(targets, target{id, it.name, it.price})
		}
	}

	if len(targets) == 0 {
		if appLogger != nil {
			appLogger.Info("送料推定バッチ", "スキップ", "送料未設定の商品なし")
		}
		return nil
	}

	if appLogger != nil {
		appLogger.Info("送料推定バッチ", "対象件数", fmt.Sprintf("%d件", len(targets)))
	}

	// ── Chrome 起動確認 ───────────────────────────────────────────────────
	if !isChromeRunning() {
		launchChrome()
		if err := waitForPort("localhost:9222", 10*time.Second); err != nil {
			return fmt.Errorf("Chrome起動タイムアウト: %w", err)
		}
	}

	// ── 各商品の送料を推定して保存 ────────────────────────────────────────
	updated := 0
	for i, t := range targets {
		if appLogger != nil {
			appLogger.Info("送料推定バッチ",
				fmt.Sprintf("(%d/%d)", i+1, len(targets)),
				fmt.Sprintf("[%s] %s ¥%d", t.id, t.name, t.price))
		}

		est, err := EstimateShipping(t.name, t.price)
		if err != nil || est.Confidence == "none" {
			if appLogger != nil {
				appLogger.Warn("送料推定バッチ", t.id, "推定失敗 → スキップ")
			}
			continue
		}

		c := cb[t.id]
		c.ShippingCost = est.Cost
		c.UpdatedAt = time.Now()
		cb[t.id] = c

		if err := costs.Save(cb); err != nil {
			if appLogger != nil {
				appLogger.Warn("送料推定バッチ", t.id, "costs.json 保存失敗: "+err.Error())
			}
		} else {
			updated++
		}

		if appLogger != nil {
			appLogger.Info("送料推定バッチ", t.id,
				fmt.Sprintf("→ %s ¥%d (信頼度:%s, %d件参照)", est.Method, est.Cost, est.Confidence, est.SampleSize))
		}

		time.Sleep(500 * time.Millisecond)
	}

	if appLogger != nil {
		appLogger.Info("送料推定バッチ", "完了", fmt.Sprintf("%d/%d件 推定済み", updated, len(targets)))
	}
	return nil
}
