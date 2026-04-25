package main

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/cdproto/target"
	"github.com/chromedp/chromedp"
)

// 必要なフィールドのみ定義
type TabInfo struct {
	ID  string `json:"id"`
	URL string `json:"url"`
}

func main() {
	var err error
	appLogger, err = NewAppLogger()
	if err != nil {
		log.Fatalf("ロガー初期化失敗: %v", err)
	}
	defer appLogger.Close()

	appLogger.Separator()
	appLogger.Info("起動", fmt.Sprintf("バージョン %s 開始", Version), "OK")

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

	appLogger.Info("価格ログ", "スナップショット記録", "開始 (値引き前)")
	if err := logPrice(ctx, itemIDs); err != nil {
		appLogger.Error("価格ログ", "スナップショット記録", fmt.Sprintf("失敗: %v", err))
	}

	appLogger.Info("値引き処理", "一括値引き", "開始")
	if err := discountPrices(ctx, itemIDs); err != nil {
		appLogger.Error("値引き処理", "一括値引き", fmt.Sprintf("失敗: %v", err))
	} else {
		appLogger.Info("値引き処理", "一括値引き", "完了")
	}

	appLogger.Info("価格ログ", "スナップショット記録", "開始 (値引き後)")
	if err := logPrice(ctx, itemIDs); err != nil {
		appLogger.Error("価格ログ", "スナップショット記録", fmt.Sprintf("失敗: %v", err))
	}

	appLogger.Info("終了", "全処理完了", "OK")
	appLogger.Separator()
}

// logPrice は各商品の現在価格をCSVに記録します。
func logPrice(ctx context.Context, ids []string) error {
	targetURL := "https://jp.mercari.com/mypage/listings"
	statusCode, err := navigateWithStatus(ctx, targetURL,
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
		id    string
		name  string
		price int
	}
	var rows []itemRow

	for i := 0; i < itemCount; i++ {
		var href, name, priceText string
		selPrefix := fmt.Sprintf(`ul[data-testid="listed-item-list"] > li:nth-child(%d)`, i+1)

		err := chromedp.Run(ctx,
			chromedp.AttributeValue(selPrefix+` a`, "href", &href, nil, chromedp.ByQuery),
			chromedp.Text(selPrefix+` p[data-testid="item-label"]`, &name, chromedp.ByQuery),
			chromedp.Text(selPrefix+` span[data-testid="price"]`, &priceText, chromedp.ByQuery),
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

		rows = append(rows, itemRow{id: itemID, name: name, price: price})
	}

	sort.Slice(rows, func(i, j int) bool {
		return rows[i].id < rows[j].id
	})

	w := csv.NewWriter(csvFile)
	defer w.Flush()
	w.Write([]string{"商品ID", "商品名", "価格(円)"})
	for _, r := range rows {
		w.Write([]string{r.id, r.name, strconv.Itoa(r.price)})
	}

	appLogger.Info("出品一覧(価格CSV)", "CSV保存完了", csvFilePath)
	return nil
}

// discountPrices は各商品を1%値引きします。
func discountPrices(ctx context.Context, ids []string) error {
	for i, id := range ids {
		screen := fmt.Sprintf("商品編集:%s", id)
		editURL := fmt.Sprintf("https://jp.mercari.com/sell/edit/%s", id)

		appLogger.Info(screen, fmt.Sprintf("(%d/%d) 画面遷移", i+1, len(ids)), editURL)

		node_price := `input[name="price"]`
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
			chromedp.Value(node_price, &priceStr, chromedp.ByQuery),
		)
		if err != nil {
			appLogger.Error(screen, "商品情報取得", fmt.Sprintf("失敗: %v", err))
			continue
		}

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

		if newPrice < MinPrice {
			appLogger.Warn(screen, "値引き判定", fmt.Sprintf("%d円 → %d円 は最低価格%d円未満のためスキップ", price, newPrice, MinPrice))
			continue
		}

		appLogger.Info(screen, "値引き実行", fmt.Sprintf("%d円 → %d円", price, newPrice))

		timeoutCtx, cancelTimeout := context.WithTimeout(ctx, 8*time.Second)
		err = chromedp.Run(timeoutCtx,
			chromedp.WaitVisible(node_price, chromedp.ByQuery),
			chromedp.Sleep(waitTime*time.Second),
			chromedp.Focus(node_price, chromedp.ByQuery),
			chromedp.SendKeys(node_price, strconv.Itoa(newPrice), chromedp.ByQuery),
			chromedp.Blur(node_price, chromedp.ByQuery),
			chromedp.Click(`button[data-testid="edit-button"]`, chromedp.ByQuery),
			chromedp.Sleep(waitTime*time.Second),
		)
		cancelTimeout()
		if err != nil {
			appLogger.Error(screen, "値引き実行", fmt.Sprintf("失敗(タイムアウト等): %v", err))
			chromedp.Run(ctx,
				chromedp.Reload(),
				chromedp.Sleep(2*time.Second),
			)
		} else {
			appLogger.Info(screen, "値引き実行", "成功")
		}
	}
	return nil
}

// navigateWithStatus はURLに遷移してHTTPステータスコードを返します。
// additionalActions に WaitVisible 等の追加アクションを渡せます。
func navigateWithStatus(ctx context.Context, targetURL string, additionalActions ...chromedp.Action) (int, error) {
	statusCh := make(chan int, 10)

	listenCtx, cancelListen := context.WithCancel(ctx)
	defer cancelListen()

	chromedp.ListenTarget(listenCtx, func(ev interface{}) {
		if e, ok := ev.(*network.EventResponseReceived); ok {
			select {
			case statusCh <- int(e.Response.Status):
			default:
			}
		}
	})

	actions := []chromedp.Action{
		network.Enable(),
		chromedp.Navigate(targetURL),
	}
	actions = append(actions, additionalActions...)

	err := chromedp.Run(ctx, actions...)

	var statusCode int
	select {
	case statusCode = <-statusCh:
	default:
		if err == nil {
			statusCode = 200
		}
	}

	return statusCode, err
}

// statusLabel はHTTPステータスコードを人間が読みやすい形に変換します。
func statusLabel(code int) string {
	switch {
	case code == 200:
		return "200 OK"
	case code == 0:
		return "ステータス取得不可"
	default:
		return fmt.Sprintf("%d", code)
	}
}

// isChromeRunning は Chrome が既に起動しているかを確認します。
func isChromeRunning() bool {
	conn, err := net.DialTimeout("tcp", "localhost:9222", 500*time.Millisecond)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

// getContext はタブIDからchromedpコンテキストを生成します。
func getContext(id string) (context.Context, context.CancelFunc, context.CancelFunc) {
	allocCtx, cancel := chromedp.NewRemoteAllocator(context.Background(), "http://localhost:9222")
	ctx, cancelCtx := chromedp.NewContext(allocCtx, chromedp.WithTargetID(target.ID(id)))
	return ctx, cancel, cancelCtx
}

// IsLoginDomain は現在のURLがログインページかを判定します。
func IsLoginDomain(ctxt context.Context) (bool, error) {
	var currentURL string
	err := chromedp.Run(ctxt,
		chromedp.Location(&currentURL),
	)
	if err != nil {
		return false, err
	}

	parsedURL, err := url.Parse(currentURL)
	if err != nil {
		return false, err
	}

	domain := parsedURL.Hostname()
	appLogger.Info("ログイン確認", "現在のドメイン", domain)

	return domain == "login.jp.mercari.com", nil
}

// NavigateToMercariMyPageListings は出品一覧ページに遷移して商品IDを返します。
func NavigateToMercariMyPageListings(ctxt context.Context) ([]string, bool) {
	var itemIDs []string
	listingsURL := "https://jp.mercari.com/mypage/listings"

	var pageTitle string
	var hrefs []map[string]string
	var currentURL string
	sel := `ul[data-testid="listed-item-list"] li a`

	statusCode, err := navigateWithStatus(ctxt, listingsURL,
		chromedp.Location(&currentURL),
		chromedp.Title(&pageTitle),
		chromedp.WaitVisible(`ul[data-testid="listed-item-list"]`, chromedp.ByQuery),
	)
	if err != nil {
		appLogger.Error("出品一覧", "画面遷移", fmt.Sprintf("失敗: %v", err))
		log.Fatalf("chromedp run error: %v", err)
	}
	appLogger.Info("出品一覧", "画面遷移", statusLabel(statusCode))
	appLogger.Info("出品一覧", "ページタイトル", pageTitle)

	if err := clickLoadMoreIfExists(ctxt, 10, 800*time.Millisecond); err != nil {
		appLogger.Warn("出品一覧", "「もっと見る」クリック", fmt.Sprintf("エラー: %v", err))
	}

	if err := chromedp.Run(ctxt,
		chromedp.AttributesAll(sel, &hrefs, chromedp.ByQueryAll),
	); err != nil {
		appLogger.Error("出品一覧", "商品リンク取得", fmt.Sprintf("失敗: %v", err))
		log.Fatalf("chromedp run error: %v", err)
	}

	if currentURL != listingsURL {
		appLogger.Warn("出品一覧", "URL確認", fmt.Sprintf("想定外のURL: %s", currentURL))
		return itemIDs, false
	}

	for _, attrs := range hrefs {
		href := attrs["href"]
		if strings.HasPrefix(href, "/item/") {
			id := strings.TrimPrefix(href, "/item/")
			itemIDs = append(itemIDs, id)
		}
	}

	appLogger.Info("出品一覧", "商品ID抽出", fmt.Sprintf("%d件", len(itemIDs)))
	return itemIDs, true
}

// GetNewTabID は操作対象のChromeタブIDを返します。
func GetNewTabID() string {
	resp, err := http.Get("http://localhost:9222/json")
	if err != nil {
		return ""
	}
	defer resp.Body.Close()

	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return ""
	}

	var tabs []TabInfo
	if err := json.Unmarshal(body, &tabs); err != nil {
		return ""
	}

	for _, tab := range tabs {
		switch tab.URL {
		case "chrome://newtab/":
			return tab.ID
		case "https://jp.mercari.com/":
			return tab.ID
		}
	}

	return ""
}

// chromeAnkerMerucari はメルカリトップページを開きます。
func chromeAnkerMerucari(ctxt context.Context) error {
	var pageTitle string
	statusCode, err := navigateWithStatus(ctxt, "https://jp.mercari.com/",
		chromedp.Title(&pageTitle),
	)
	if err != nil {
		return err
	}
	appLogger.Info("メルカリTOP", "画面遷移", statusLabel(statusCode))
	appLogger.Info("メルカリTOP", "ページタイトル", pageTitle)
	return nil
}

// launchChrome は Chrome をデバッグモードで起動します。
func launchChrome() *exec.Cmd {
	view := "--headless=new"
	var cmd *exec.Cmd
	if viewFlg {
		cmd = exec.Command(
			"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
			"--remote-debugging-port=9222",
			"--user-data-dir=/tmp/chrome-debug",
			"--no-first-run", "--no-default-browser-check",
		)
	} else {
		cmd = exec.Command(
			"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
			"--remote-debugging-port=9222",
			"--user-data-dir=/tmp/chrome-debug",
			"--no-first-run", "--no-default-browser-check",
			view,
		)
	}

	if err := cmd.Start(); err != nil {
		appLogger.Error("起動", "Chrome起動", fmt.Sprintf("失敗: %v", err))
		log.Fatalf("Failed to start Chrome: %v", err)
	}
	appLogger.Info("起動", "Chromeプロセス開始", "OK")

	if err := waitForPort("localhost:9222", 10*time.Second); err != nil {
		appLogger.Error("起動", "Chrome DevToolsポート待機", fmt.Sprintf("タイムアウト: %v", err))
		log.Fatalf("Chrome didn't open port in time: %v", err)
	}

	appLogger.Info("起動", "Chrome DevTools接続準備", "完了 (port 9222)")
	return cmd
}

// waitForPort は指定ポートが開くまで待機します。
func waitForPort(address string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		conn, err := net.DialTimeout("tcp", address, 500*time.Millisecond)
		if err == nil {
			conn.Close()
			return nil
		}
		if time.Now().After(deadline) {
			return err
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// clickLoadMoreIfExists は「もっと見る」ボタンを繰り返しクリックします。
func clickLoadMoreIfExists(ctxt context.Context, maxClicks int, wait time.Duration) error {
	jsClick := `(function(){
        const btns = Array.from(document.querySelectorAll('button'));
        for (const b of btns) {
            if (b.innerText && b.innerText.trim().indexOf('もっと見る') !== -1) {
                b.scrollIntoView();
                b.click();
                return true;
            }
        }
        return false;
    })()`

	for i := 0; i < maxClicks; i++ {
		var clicked bool
		if err := chromedp.Run(ctxt,
			chromedp.Evaluate(jsClick, &clicked),
		); err != nil {
			return err
		}

		if !clicked {
			return nil
		}

		appLogger.Info("出品一覧", fmt.Sprintf("「もっと見る」クリック (%d回目)", i+1), "OK")
		time.Sleep(wait)
	}

	return nil
}

