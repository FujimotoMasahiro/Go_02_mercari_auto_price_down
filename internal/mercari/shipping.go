package mercari

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/chromedp/chromedp"
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
// Chrome が未起動の場合は自動で起動します。
func EstimateShipping(itemName string, price int) (ShippingEstimate, error) {
	ensureLogger()

	if !isChromeRunning() {
		return ShippingEstimate{Confidence: "none"}, fmt.Errorf("Chrome未起動: 値引き実行またはリサーチを先に起動してください")
	}

	tab, err := newTabContext()
	if err != nil {
		return ShippingEstimate{Confidence: "none"}, fmt.Errorf("タブ作成失敗: %w", err)
	}
	defer tab.Close()

	ctx, cancel := context.WithTimeout(tab.ctx, 90*time.Second)
	defer cancel()

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

	const extractJS = `(function() {
		const cards = Array.from(document.querySelectorAll('a[href*="/item/"]'));
		return JSON.stringify(cards.slice(0, 20).map(a => {
			const id = (a.href.match(/\/item\/(m\w+)/) || [])[1] || '';
			const name = (a.querySelector('[class*="itemName"],[class*="name"]') || {}).textContent || '';
			const priceText = (a.querySelector('[class*="price"]') || {}).textContent || '';
			return { id, name: name.trim(), priceText: priceText.trim() };
		}).filter(x => x.id));
	})()`

	// WaitVisible がタイムアウトしても固定待機後に抽出を試みる
	navigateWithStatus(ctx, searchURL) //nolint
	time.Sleep(2 * time.Second)

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
		return nil, err
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
		if score >= 2 {
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
