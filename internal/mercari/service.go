package mercari

import (
	"context"
	"encoding/csv"
	"fmt"
	"log"
	"math"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/chromedp"

	"mercari-pricelower/internal/config"
	"mercari-pricelower/internal/history"
	"mercari-pricelower/internal/logger"
)

var appLogger *logger.AppLogger

// RunDiscount は値引き処理の全フローを実行します（CSV前後スナップショット込み）。
// excludedIDs に含まれる商品IDは値引き対象外となります。
func RunDiscount(excludedIDs []string) {
	run(func(ctx context.Context, itemIDs []string) {
		appLogger.Info("価格ログ", "スナップショット記録", "開始 (値引き前)")
		if err := logPrice(ctx, itemIDs); err != nil {
			appLogger.Error("価格ログ", "スナップショット記録", fmt.Sprintf("失敗: %v", err))
		}

		excluded := make(map[string]bool, len(excludedIDs))
		for _, id := range excludedIDs {
			excluded[id] = true
		}
		var targetIDs []string
		for _, id := range itemIDs {
			if excluded[id] {
				appLogger.Info("値引き処理", fmt.Sprintf("商品%s", id), "対象外指定のためスキップ")
			} else {
				targetIDs = append(targetIDs, id)
			}
		}

		appLogger.Info("値引き処理", "一括値引き", fmt.Sprintf("開始 (%d件 / 除外%d件)", len(targetIDs), len(excludedIDs)))
		discounts, err := discountPrices(ctx, targetIDs)
		if err != nil {
			appLogger.Error("値引き処理", "一括値引き", fmt.Sprintf("失敗: %v", err))
		} else {
			appLogger.Info("値引き処理", "一括値引き", "完了")
		}
		if len(discounts) > 0 {
			if err := history.Append(history.Entry{
				Timestamp: time.Now(),
				Products:  discounts,
			}); err != nil {
				appLogger.Warn("値引き処理", "履歴記録", fmt.Sprintf("失敗: %v", err))
			}
		}

		appLogger.Info("価格ログ", "スナップショット記録", "開始 (値引き後)")
		if err := logPrice(ctx, itemIDs); err != nil {
			appLogger.Error("価格ログ", "スナップショット記録", fmt.Sprintf("失敗: %v", err))
		}
	})
}

// RunCSVOnly はCSV作成と画像保存のみを実行します（値引きなし）。
func RunCSVOnly() {
	run(func(ctx context.Context, itemIDs []string) {
		appLogger.Info("価格ログ", "スナップショット記録", "開始 (CSV作成モード)")
		if err := logPrice(ctx, itemIDs); err != nil {
			appLogger.Error("価格ログ", "スナップショット記録", fmt.Sprintf("失敗: %v", err))
		}
	})
}

// run はChrome起動・認証確認・商品ID取得などの共通フローを実行し、fn を呼び出します。
func run(fn func(ctx context.Context, itemIDs []string)) {
	var err error
	appLogger, err = logger.New()
	if err != nil {
		log.Fatalf("ロガー初期化失敗: %v", err)
	}
	defer appLogger.Close()

	appLogger.Separator()
	appLogger.Info("起動", fmt.Sprintf("バージョン %s 開始", config.Version), "OK")

	var cmd *exec.Cmd
	if !isChromeRunning() {
		appLogger.Info("起動", "Chrome起動", "開始")
		cmd = launchChrome()
		defer cmd.Process.Kill()
	} else {
		appLogger.Info("起動", "Chrome接続確認", "既に起動中")
	}

	id := GetNewTabID()
	if id == "" {
		appLogger.Error("起動", "タブID取得", "タブが見つかりません")
		return
	}
	appLogger.Info("起動", "タブID取得", id)

	ctx, cancel1, cancel2 := getContext(id)
	defer cancel1()
	defer cancel2()

	if err := chromeAnkerMerucari(ctx); err != nil {
		appLogger.Error("メルカリTOP", "画面遷移", fmt.Sprintf("失敗: %v", err))
		return
	}

	itemIDs, ok := NavigateToMercariMyPageListings(ctx)
	appLogger.Info("出品一覧", "商品数取得", fmt.Sprintf("%d件", len(itemIDs)))
	if !ok {
		appLogger.Warn("出品一覧", "画面遷移確認", "出品一覧ページではない可能性あり")
	}

	if ng, err := IsLoginDomain(ctx); err != nil {
		appLogger.Error("ログイン確認", "ドメイン確認", fmt.Sprintf("エラー: %v", err))
		return
	} else if ng {
		appLogger.Warn("ログイン確認", "ログイン状態", "未ログイン - 手動ログインが必要です")
		return
	}
	appLogger.Info("ログイン確認", "ログイン状態", "ログイン済み")

	fn(ctx, itemIDs)

	appLogger.Info("終了", "全処理完了", "OK")
	appLogger.Separator()
}

func logPrice(ctx context.Context, ids []string) error {
	var mu sync.Mutex
	type reqInfo struct {
		url      string
		status   int
		finished bool
	}
	reqByID := make(map[network.RequestID]*reqInfo)
	reqByURL := make(map[string]network.RequestID)

	chromedp.ListenTarget(ctx, func(ev interface{}) {
		switch e := ev.(type) {
		case *network.EventRequestWillBeSent:
			mu.Lock()
			reqByID[e.RequestID] = &reqInfo{url: e.Request.URL}
			reqByURL[e.Request.URL] = e.RequestID
			mu.Unlock()
		case *network.EventResponseReceived:
			mu.Lock()
			if s, ok := reqByID[e.RequestID]; ok {
				s.status = int(e.Response.Status)
			}
			mu.Unlock()
		case *network.EventLoadingFinished:
			mu.Lock()
			if s, ok := reqByID[e.RequestID]; ok {
				s.finished = true
			}
			mu.Unlock()
		}
	})

	statusCode, err := navigateWithStatus(ctx, "https://jp.mercari.com/mypage/listings",
		chromedp.WaitVisible(`ul[data-testid="listed-item-list"]`, chromedp.ByQuery),
	)
	if err != nil {
		appLogger.Error("出品一覧(価格CSV)", "画面遷移", fmt.Sprintf("失敗: %v", err))
		return fmt.Errorf("一覧ページの読み込み失敗: %w", err)
	}
	appLogger.Info("出品一覧(価格CSV)", "画面遷移", statusLabel(statusCode))

	idSet := make(map[string]bool)
	for _, id := range ids {
		idSet[id] = true
	}

	var itemCount int
	if err := chromedp.Run(ctx,
		chromedp.Evaluate(`document.querySelectorAll('ul[data-testid="listed-item-list"] > li').length`, &itemCount),
	); err != nil {
		appLogger.Error("出品一覧(価格CSV)", "商品数取得", fmt.Sprintf("失敗: %v", err))
		return fmt.Errorf("商品数の取得失敗: %w", err)
	}
	appLogger.Info("出品一覧(価格CSV)", "商品数取得", fmt.Sprintf("%d件", itemCount))

	var bodyHeight, viewHeight float64
	chromedp.Run(ctx,
		chromedp.Evaluate(`document.body.scrollHeight`, &bodyHeight),
		chromedp.Evaluate(`window.innerHeight`, &viewHeight),
	)
	if viewHeight <= 0 {
		viewHeight = 800
	}
	appLogger.Info("出品一覧(価格CSV)", "スクロール開始", fmt.Sprintf("ページ高さ: %.0fpx", bodyHeight))
	for y := 0.0; y <= bodyHeight; y += viewHeight {
		chromedp.Run(ctx, chromedp.Evaluate(fmt.Sprintf(`window.scrollTo(0, %d)`, int(y)), nil))
		time.Sleep(200 * time.Millisecond)
	}

	appLogger.Info("出品一覧(価格CSV)", "画像ロード待機", "開始")
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		var allComplete bool
		chromedp.Run(ctx, chromedp.Evaluate(`
			Array.from(document.querySelectorAll('ul[data-testid="listed-item-list"] img'))
				.every(img => img.complete && img.naturalWidth > 0)
		`, &allComplete))
		if allComplete {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	appLogger.Info("出品一覧(価格CSV)", "画像ロード待機", "完了")

	csvDir := "CSV"
	if err := os.MkdirAll(csvDir, 0755); err != nil {
		appLogger.Error("出品一覧(価格CSV)", "CSVディレクトリ作成", fmt.Sprintf("失敗: %v", err))
		return fmt.Errorf("CSVディレクトリ作成失敗: %w", err)
	}
	csvFileName := time.Now().Format("20060102150405") + ".csv"
	csvFilePath := filepath.Join(csvDir, csvFileName)
	csvFile, err := os.Create(csvFilePath)
	if err != nil {
		appLogger.Error("出品一覧(価格CSV)", "CSVファイル作成", fmt.Sprintf("失敗: %v", err))
		return fmt.Errorf("CSVファイル作成失敗: %w", err)
	}
	defer csvFile.Close()

	type itemRow struct {
		id     string
		name   string
		price  int
		imgSrc string
	}
	var rows []itemRow

	for i := 0; i < itemCount; i++ {
		var href, name, priceText, imgSrc string
		selPrefix := fmt.Sprintf(`ul[data-testid="listed-item-list"] > li:nth-child(%d)`, i+1)

		err := chromedp.Run(ctx,
			chromedp.AttributeValue(selPrefix+` a`, "href", &href, nil, chromedp.ByQuery),
			chromedp.Text(selPrefix+` p[data-testid="item-label"]`, &name, chromedp.ByQuery),
			chromedp.Text(selPrefix+` span[data-testid="price"]`, &priceText, chromedp.ByQuery),
			chromedp.Evaluate(fmt.Sprintf(`(function(){const img=document.querySelector(%q);return img?(img.src||img.dataset.src||''):'';})()`, selPrefix+` img`), &imgSrc),
		)
		if err != nil {
			appLogger.Warn("出品一覧(価格CSV)", fmt.Sprintf("商品%d情報取得", i+1), fmt.Sprintf("失敗: %v", err))
			continue
		}

		if !strings.HasPrefix(href, "/item/") {
			continue
		}
		itemID := strings.TrimPrefix(href, "/item/")
		if !idSet[itemID] {
			continue
		}

		priceText = strings.ReplaceAll(priceText, ",", "")
		priceText = strings.ReplaceAll(priceText, "円", "")
		priceText = strings.ReplaceAll(priceText, "¥\n", "")
		priceText = strings.TrimSpace(priceText)
		price, err := strconv.Atoi(priceText)
		if err != nil {
			appLogger.Warn("出品一覧(価格CSV)", fmt.Sprintf("商品%s 価格パース", itemID), fmt.Sprintf("失敗: %v", err))
			continue
		}

		rows = append(rows, itemRow{id: itemID, name: name, price: price, imgSrc: imgSrc})
	}

	sort.Slice(rows, func(i, j int) bool { return rows[i].id < rows[j].id })

	w := csv.NewWriter(csvFile)
	defer w.Flush()
	w.Write([]string{"商品ID", "商品名", "価格(円)"})
	for _, r := range rows {
		w.Write([]string{r.id, r.name, strconv.Itoa(r.price)})
	}
	appLogger.Info("出品一覧(価格CSV)", "CSV保存完了", csvFilePath)

	imgDir := "img"
	if err := os.MkdirAll(imgDir, 0755); err != nil {
		appLogger.Warn("出品一覧(画像保存)", "imgディレクトリ作成", fmt.Sprintf("失敗: %v", err))
		return nil
	}

	for _, r := range rows {
		if r.imgSrc == "" {
			continue
		}
		ext := imageExt(r.imgSrc)
		imgPath := filepath.Join(imgDir, r.id+ext)
		if _, err := os.Stat(imgPath); err == nil {
			appLogger.Info("出品一覧(画像保存)", fmt.Sprintf("商品%s", r.id), "スキップ(既存)")
			continue
		}

		mu.Lock()
		reqID, found := reqByURL[r.imgSrc]
		var state *reqInfo
		if found {
			state = reqByID[reqID]
		}
		mu.Unlock()

		if !found || state == nil || state.status != 200 || !state.finished {
			appLogger.Warn("出品一覧(画像保存)", fmt.Sprintf("商品%s", r.id), "HTTP200未確認のためスキップ")
			continue
		}

		var body []byte
		err := chromedp.Run(ctx, chromedp.ActionFunc(func(ctx context.Context) error {
			var e error
			body, e = network.GetResponseBody(reqID).Do(ctx)
			return e
		}))
		if err != nil {
			appLogger.Warn("出品一覧(画像保存)", fmt.Sprintf("商品%s", r.id), fmt.Sprintf("レスポンスボディ取得失敗: %v", err))
			continue
		}
		if err := os.WriteFile(imgPath, body, 0644); err != nil {
			appLogger.Warn("出品一覧(画像保存)", fmt.Sprintf("商品%s", r.id), fmt.Sprintf("保存失敗: %v", err))
		} else {
			appLogger.Info("出品一覧(画像保存)", fmt.Sprintf("商品%s", r.id), imgPath)
		}
	}

	return nil
}

func discountPrices(ctx context.Context, ids []string) ([]history.ProductDiscount, error) {
	var discounts []history.ProductDiscount
	for i, id := range ids {
		screen := fmt.Sprintf("商品編集:%s", id)
		editURL := fmt.Sprintf("https://jp.mercari.com/sell/edit/%s", id)

		appLogger.Info(screen, fmt.Sprintf("(%d/%d) 画面遷移", i+1, len(ids)), editURL)

		nodePriceInput := `input[name="price"]`
		var hasActivateBtn bool
		var priceStr string

		statusCode, err := navigateWithStatus(ctx, editURL,
			chromedp.WaitVisible(`body`, chromedp.ByQuery),
		)
		if err != nil {
			appLogger.Error(screen, "画面遷移", fmt.Sprintf("失敗: %v", err))
			continue
		}
		appLogger.Info(screen, "画面遷移", statusLabel(statusCode))

		err = chromedp.Run(ctx,
			chromedp.EvaluateAsDevTools(
				`document.querySelector('button[data-testid="activate-button"]') !== null`,
				&hasActivateBtn,
			),
			chromedp.Value(nodePriceInput, &priceStr, chromedp.ByQuery),
		)
		if err != nil {
			appLogger.Error(screen, "商品情報取得", fmt.Sprintf("失敗: %v", err))
			continue
		}

		var itemName string
		chromedp.Run(ctx, chromedp.EvaluateAsDevTools(
			`(function(){ var el = document.querySelector('textarea[name="name"]') || document.querySelector('input[name="name"]'); return el ? el.value : ''; })()`,
			&itemName,
		))
		itemName = strings.TrimSpace(itemName)

		if hasActivateBtn {
			appLogger.Warn(screen, "出品状態確認", "非公開のためスキップ")
			continue
		}

		priceStr = strings.TrimSpace(priceStr)
		price, err := strconv.Atoi(priceStr)
		if err != nil {
			appLogger.Error(screen, "現在価格取得", fmt.Sprintf("パース失敗: %v", err))
			continue
		}

		newPrice := int(math.Round(float64(price) / 100 * 99))
		if newPrice < config.MinPrice {
			appLogger.Warn(screen, "値引き判定", fmt.Sprintf("%d円 → %d円 は最低価格%d円未満のためスキップ", price, newPrice, config.MinPrice))
			continue
		}

		appLogger.Info(screen, "値引き実行", fmt.Sprintf("%d円 → %d円", price, newPrice))

		timeoutCtx, cancelTimeout := context.WithTimeout(ctx, 8*time.Second)
		err = chromedp.Run(timeoutCtx,
			chromedp.WaitVisible(nodePriceInput, chromedp.ByQuery),
			chromedp.Sleep(config.WaitTime*time.Second),
			chromedp.Focus(nodePriceInput, chromedp.ByQuery),
			chromedp.SendKeys(nodePriceInput, strconv.Itoa(newPrice), chromedp.ByQuery),
			chromedp.Blur(nodePriceInput, chromedp.ByQuery),
			chromedp.Click(`button[data-testid="edit-button"]`, chromedp.ByQuery),
			chromedp.Sleep(config.WaitTime*time.Second),
		)
		cancelTimeout()
		if err != nil {
			appLogger.Error(screen, "値引き実行", fmt.Sprintf("失敗(タイムアウト等): %v", err))
			chromedp.Run(ctx, chromedp.Reload(), chromedp.Sleep(2*time.Second))
		} else {
			appLogger.Info(screen, "値引き実行", "成功")
			discounts = append(discounts, history.ProductDiscount{
				ItemID:   id,
				ItemName: itemName,
				OldPrice: price,
				NewPrice: newPrice,
			})
		}
	}
	return discounts, nil
}

// imageExt は画像URLから拡張子を取得します。
func imageExt(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil || u.Path == "" {
		return ".jpg"
	}
	ext := strings.ToLower(filepath.Ext(u.Path))
	switch ext {
	case ".jpg", ".jpeg", ".png", ".webp", ".gif":
		return ext
	}
	return ".jpg"
}
