package main

import (
	"context"
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
	"strconv"
	"strings"
	"time"

	"github.com/chromedp/cdproto/target"
	"github.com/chromedp/chromedp"
)

// 必要なフィールドのみ定義
type TabInfo struct {
	ID  string `json:"id"`
	URL string `json:"url"`
}

func main() {
	fmt.Println("App Version:", Version)

	var cmd *exec.Cmd

	// Chromeが既に開かれている場合はスキップする
	if !isChromeRunning() {
		// Chromeを開く
		cmd = launchChrome()
		defer cmd.Process.Kill()
	}

	// 開いているタブのIDを取得
	id := GetNewTabID()

	//　タブ情報を元に操作用のタブを作製
	ctx, cancel1, cancel2 := getContext(id)
	defer cancel1()
	defer cancel2()

	// メルカリにログイン(手動)
	chromeAnkerMerucari(ctx)

	// 出品した商品一覧画面に遷移する
	itemIDs, _ := NavigateToMercariMyPageListings(ctx)
	fmt.Println("対象商品数:", len(itemIDs))

	// ログイン画面が表示された場合は処理を中断する（手動ログインさせる）
	if ng, err := IsLoginDomain(ctx); err != nil {
		log.Println("エラー:", err)
		return
	} else if ng {
		log.Println("未ログインのためログインしてください")
		return
	}

	logPrice(ctx, itemIDs)
	discountPrices(ctx, itemIDs)
	logPrice(ctx, itemIDs)
}

// logPrice は、与えられたメルカリ商品IDの一覧に対して、
// 各商品の現在の価格情報をログファイルに記録する関数です。
func logPrice(ctx context.Context, ids []string) error {
	fmt.Printf("出品中の商品一覧からログファイル作成開始\n")
	logDir := "MerucariLog"
	if err := os.MkdirAll(logDir, 0755); err != nil {
		return err
	}
	logFileName := time.Now().Format("20060102150405") + ".log"
	logFilePath := filepath.Join(logDir, logFileName)
	logFile, err := os.Create(logFilePath)
	if err != nil {
		return err
	}
	defer logFile.Close()
	log.SetOutput(logFile)

	// 対象IDをセットで保持
	idSet := make(map[string]bool)
	for _, id := range ids {
		idSet[id] = true
	}

	// 一覧ページに遷移
	url := "https://jp.mercari.com/mypage/listings"
	if err := chromedp.Run(ctx,
		chromedp.Navigate(url),
		chromedp.WaitVisible(`ul[data-testid="listed-item-list"]`, chromedp.ByQuery),
	); err != nil {
		return fmt.Errorf("一覧ページの読み込み失敗: %w", err)
	}

	// li要素の数を取得
	var itemCount int
	if err := chromedp.Run(ctx,
		chromedp.Evaluate(`document.querySelectorAll('ul[data-testid="listed-item-list"] > li').length`, &itemCount),
	); err != nil {
		return fmt.Errorf("商品数の取得失敗: %w", err)
	}

	// 1商品ずつ処理
	for i := 0; i < itemCount; i++ {

		var href, name, priceText string
		selPrefix := fmt.Sprintf(`ul[data-testid="listed-item-list"] > li:nth-child(%d)`, i+1)

		err := chromedp.Run(ctx,
			chromedp.AttributeValue(selPrefix+` a`, "href", &href, nil, chromedp.ByQuery),
			chromedp.Text(selPrefix+` p[data-testid="item-label"]`, &name, chromedp.ByQuery),
			chromedp.Text(selPrefix+` span[data-testid="price"]`, &priceText, chromedp.ByQuery),
		)
		if err != nil {
			log.Printf("商品 %d の取得エラー: %v\n", i, err)
			continue
		}

		// IDを抽出して対象か確認
		if !strings.HasPrefix(href, "/item/") {
			continue
		}
		id := strings.TrimPrefix(href, "/item/")
		if !idSet[id] {
			continue
		}

		// 価格パース
		priceText = strings.ReplaceAll(priceText, ",", "")
		priceText = strings.ReplaceAll(priceText, "円", "")
		priceText = strings.ReplaceAll(priceText, "¥\n", "")
		priceText = strings.TrimSpace(priceText)
		price, err := strconv.Atoi(priceText)
		if err != nil {
			log.Printf("商品 %s の価格取得失敗: %v\n", id, err)
			continue
		}

		log.Printf("商品 %s：商品名「%s」 現在価格（Before）=%d円\n", id, name, price)
	}

	fmt.Printf("出品中の商品一覧からログファイル作成終了\n")
	return nil
}

// discountPrices は、与えられたメルカリ商品IDの一覧に対して、
// 各商品の価格を値引き（ただし最低価格未満にはしない）し、保存する関数です。
// 非公開の商品（「出品を再開する」ボタンが表示されている商品）はスキップされます。
func discountPrices(ctx context.Context, ids []string) error {
	count := 0
	for _, id := range ids {
		count++
		fmt.Printf("対象商品数: %d\n", count)
		// 編集画面URLを生成
		url := fmt.Sprintf("https://jp.mercari.com/sell/edit/%s", id)
		fmt.Printf("Processing %s\n", url)

		// 価格入力欄のセレクタ
		node_price := `input[name="price"]`

		// 出品停止中かどうかの判定フラグ、および現在の価格
		var hasActivateBtn bool
		var priceStr string

		// ページ遷移＆状態取得だけ行う
		err := chromedp.Run(ctx,
			chromedp.Navigate(url),
			chromedp.WaitVisible(`body`, chromedp.ByQuery),
			chromedp.EvaluateAsDevTools(
				`document.querySelector('button[data-testid="activate-button"]') !== null`,
				&hasActivateBtn,
			),
			chromedp.Value(node_price, &priceStr, chromedp.ByQuery),
		)
		if err != nil {
			log.Printf("商品 %s の処理中にエラー発生: %v\n", id, err)
			continue
		}

		// 非公開の場合はスキップ
		if hasActivateBtn {
			fmt.Printf("商品 %s は非公開のためスキップします\n", id)
			continue
		}
		// 価格文字列を整数に変換
		priceStr = strings.TrimSpace(priceStr)
		price, err := strconv.Atoi(priceStr)
		if err != nil {
			log.Printf("商品 %s の価格取得失敗: %v\n", id, err)
			continue
		}

		// 新しい価格を計算（値引き、ただし最低価格未満にはしない）
		newPrice := int(math.Round(float64(price) / 100 * 99))

		fmt.Printf("商品 %s の価格を %d → %d に値引きします\n", id, price, newPrice)

		timeoutCtx, cancelTimeout := context.WithTimeout(ctx, 8*time.Second)
		defer cancelTimeout()

		// 新しい価格を入力して「変更する」ボタンをクリック
		err = chromedp.Run(timeoutCtx,
			// 表示を待つ
			chromedp.WaitVisible(node_price, chromedp.ByQuery),

			// 少し待機（適宜調整）
			chromedp.Sleep(waitTime*time.Second),

			// 対象の入力欄を focus（反応を起こさせる）
			chromedp.Focus(node_price, chromedp.ByQuery),

			// // 新しい値を入力
			chromedp.SendKeys(node_price, strconv.Itoa(newPrice), chromedp.ByQuery),

			// blur イベントで「入力終了」処理を発火させる
			chromedp.Blur(node_price, chromedp.ByQuery),

			// 「変更する」ボタンをクリック
			chromedp.Click(`button[data-testid="edit-button"]`, chromedp.ByQuery),

			// 少し待機（適宜調整）
			chromedp.Sleep(waitTime*time.Second),
		)
		if err != nil {
			fmt.Printf("商品 %s の価格変更時にエラー: %v\n", id, err)
			// タイムアウト or 失敗
			chromedp.Run(ctx,
				chromedp.Reload(),
				chromedp.Sleep(2*time.Second),
			)
			continue
		} else {
			// 正常に変更されたことを出力
			fmt.Printf("商品 %s の価格変更完了\n", id)
		}
	}
	return nil
}

// すでにChromeが起動しているかどうかを確認する
func isChromeRunning() bool {
	conn, err := net.DialTimeout("tcp", "localhost:9222", 500*time.Millisecond)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

// タブ情報のコンテキストを取得する
func getContext(id string) (context.Context, context.CancelFunc, context.CancelFunc) {
	// リモートデバッグポートに接続
	allocCtx, cancel := chromedp.NewRemoteAllocator(context.Background(), "http://localhost:9222")

	// タブIDでContext作成
	ctx, cancelCtx := chromedp.NewContext(allocCtx, chromedp.WithTargetID(target.ID(id)))

	return ctx, cancel, cancelCtx
}

// 指定したタブのURLが login.jp.mercari.com ドメインかどうかを判定する
func IsLoginDomain(ctxt context.Context) (bool, error) {
	// 現在のURLを取得
	var currentURL string
	err := chromedp.Run(ctxt,
		chromedp.Location(&currentURL),
	)
	if err != nil {
		return false, err
	}

	// URLをパースしてドメインを判定
	parsedURL, err := url.Parse(currentURL)
	if err != nil {
		return false, err
	}

	domain := parsedURL.Hostname()
	log.Printf("現在のドメイン: %s", domain)

	// ドメインが login.jp.mercari.com の場合は true
	return domain == "login.jp.mercari.com", nil
}

// MercariMyPageListingsへ遷移する関数
func NavigateToMercariMyPageListings(ctxt context.Context) ([]string, bool) {

	var itemIDs []string

	// タスク：指定URLにアクセスしタイトルを取得する例
	var pageTitle string
	var hrefs []map[string]string
	var currentURL string
	sel := `ul[data-testid="listed-item-list"] li a`

	// ページ遷移して一覧が表示されるのを待つ
	if err := chromedp.Run(ctxt,
		chromedp.Navigate("https://jp.mercari.com/mypage/listings"),
		chromedp.Location(&currentURL),
		chromedp.Title(&pageTitle),
		chromedp.WaitVisible(`ul[data-testid="listed-item-list"]`, chromedp.ByQuery),
	); err != nil {
		log.Fatalf("chromedp run error: %v", err)
	}

	// 「もっと見る」ボタンがあればクリックして追加アイテムを読み込む（関数化）
	// maxClicks: 最大クリック回数、wait: クリック後の待機時間
	if err := clickLoadMoreIfExists(ctxt, 10, 800*time.Millisecond); err != nil {
		log.Printf("もっと見るクリック処理でエラー: %v", err)
	}

	// 最終的な a タグ属性を取得
	if err := chromedp.Run(ctxt,
		chromedp.AttributesAll(sel, &hrefs, chromedp.ByQueryAll), // aタグの属性取得
	); err != nil {
		log.Fatalf("chromedp run error: %v", err)
	}

	if currentURL != "https://jp.mercari.com/mypage/listings" {
		return itemIDs, false
	}

	log.Printf("Page title: %s", pageTitle)

	// 商品ID抽出
	for _, attrs := range hrefs {
		href := attrs["href"]
		if strings.HasPrefix(href, "/item/") {
			id := strings.TrimPrefix(href, "/item/")
			itemIDs = append(itemIDs, id)
		}
	}

	return itemIDs, true
}

// chrome://newtab/ の id を取得する関数（string型で返す）
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

	// 該当なし
	return ""
}

// メルカリを開く
func chromeAnkerMerucari(ctxt context.Context) {

	// タスク：指定URLにアクセスしタイトルを取得する例
	var pageTitle string
	err := chromedp.Run(ctxt,
		chromedp.Navigate("https://jp.mercari.com/"),
		chromedp.Title(&pageTitle),
	)
	if err != nil {
		log.Fatalf("chromedp run error: %v", err)
	}

	log.Printf("Page title: %s", pageTitle)
}

// Chromeを起動する関数
func launchChrome() *exec.Cmd {

	view := "--headless=new" // ← これがウィンドウ非表示
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
		log.Fatalf("Failed to start Chrome: %v", err)
	}
	log.Println("Chrome process started...")

	// ポートを指定してChromeを起動
	if err := waitForPort("localhost:9222", 10*time.Second); err != nil {
		log.Fatalf("Chrome didn't open port in time: %v", err)
	}

	log.Println("Chrome is ready to accept DevTools Protocol connections.")
	return cmd
}

// waitForPort は、指定したアドレスのポートが開くまで待機するユーティリティ関数です。
func waitForPort(address string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		conn, err := net.DialTimeout("tcp", address, 500*time.Millisecond)
		if err == nil {
			conn.Close()
			return nil // 接続できたら完了
		}
		if time.Now().After(deadline) {
			return err
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// clickLoadMoreIfExists は、ページ内に「もっと見る」ボタンがあればクリックして
// 追加のアイテムを読み込むユーティリティ関数です。
// - ctxt: chromedp のコンテキスト
// - maxClicks: 安全のための最大クリック回数
// - wait: クリック後に待機する時間（読み込み待ち）
func clickLoadMoreIfExists(ctxt context.Context, maxClicks int, wait time.Duration) error {
	// ボタンの有無判定＆クリックを行う簡単な JS を用意
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
		// JS を評価してボタンをクリック（見つからなければ false が返る）
		if err := chromedp.Run(ctxt,
			chromedp.Evaluate(jsClick, &clicked),
		); err != nil {
			return err
		}

		// ボタンがなければ終了
		if !clicked {
			return nil
		}

		// クリック後に読み込み待ち（必要であれば増やす）
		time.Sleep(wait)
	}

	// 最大回数に到達して終了
	return nil
}

// ...existing code...
