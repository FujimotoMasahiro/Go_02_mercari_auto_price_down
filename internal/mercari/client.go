package mercari

import (
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/cdproto/target"
	"github.com/chromedp/chromedp"

	"mercari-pricelower/internal/config"
)

func init() {
	log.SetOutput(&filteredWriter{w: os.Stderr})
}

type filteredWriter struct {
	w *os.File
}

func (fw *filteredWriter) Write(p []byte) (n int, err error) {
	if strings.Contains(string(p), "DOM.documentUpdated") {
		return len(p), nil
	}
	return fw.w.Write(p)
}

// TabInfo はChrome DevTools のタブ情報を表します。
type TabInfo struct {
	ID  string `json:"id"`
	URL string `json:"url"`
}

func isChromeRunning() bool {
	conn, err := net.DialTimeout("tcp", "localhost:9222", 500*time.Millisecond)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

func launchChrome() *exec.Cmd {
	args := []string{
		"--remote-debugging-port=9222",
		"--user-data-dir=/tmp/chrome-debug",
		"--no-first-run", "--no-default-browser-check",
	}
	if config.Cfg.Headless {
		args = append(args, "--headless=new")
	}
	cmd := exec.Command("/Applications/Google Chrome.app/Contents/MacOS/Google Chrome", args...)

	if err := cmd.Start(); err != nil {
		appLogger.Error("起動", "Chrome起動", "失敗: "+err.Error())
		log.Fatalf("Failed to start Chrome: %v", err)
	}
	appLogger.Info("起動", "Chromeプロセス開始", "OK")

	if err := waitForPort("localhost:9222", 10*time.Second); err != nil {
		appLogger.Error("起動", "Chrome DevToolsポート待機", "タイムアウト: "+err.Error())
		log.Fatalf("Chrome didn't open port in time: %v", err)
	}

	appLogger.Info("起動", "Chrome DevTools接続準備", "完了 (port 9222)")
	return cmd
}

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

// GetNewTabID はChrome DevTools から操作対象タブのIDを返します。
// chrome://newtab/ または jp.mercari.com のタブが優先されます。
// 見つからない場合（前の処理が別URLで止まっている場合など）は
// 新規タブを作成して返します。
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

	// 既存タブが見つからない場合は新規タブを作成して使用する。
	// 前回の処理が別URL（編集ページ等）で止まっていても影響を受けない。
	appLogger.Info("起動", "タブ作成", "既存タブが見つからないため新規タブを作成します")
	return openNewTab()
}

// openNewTab は Chrome DevTools API で新規タブを開き、そのIDを返します。
// Chrome 112以降は GET /json/new が非推奨のため PUT を使用します。
func openNewTab() string {
	req, err := http.NewRequest(http.MethodPut, "http://localhost:9222/json/new", nil)
	if err != nil {
		appLogger.Error("起動", "新規タブ作成", "リクエスト生成失敗: "+err.Error())
		return ""
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		appLogger.Error("起動", "新規タブ作成", "失敗: "+err.Error())
		return ""
	}
	defer resp.Body.Close()

	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		appLogger.Error("起動", "新規タブ作成", "レスポンス読み込み失敗: "+err.Error())
		return ""
	}

	var tab TabInfo
	if err := json.Unmarshal(body, &tab); err != nil {
		appLogger.Error("起動", "新規タブ作成", "JSONパース失敗: "+err.Error()+" body="+string(body))
		return ""
	}

	if tab.ID == "" {
		appLogger.Error("起動", "新規タブ作成", "タブIDが空 body="+string(body))
		return ""
	}

	// タブが準備完了するまで少し待機
	time.Sleep(500 * time.Millisecond)
	appLogger.Info("起動", "新規タブ作成", "完了 → "+tab.ID)
	return tab.ID
}

func getContext(id string) (context.Context, context.CancelFunc, context.CancelFunc) {
	allocCtx, cancel := chromedp.NewRemoteAllocator(context.Background(), "http://localhost:9222")
	ctx, cancelCtx := chromedp.NewContext(allocCtx, chromedp.WithTargetID(target.ID(id)))
	return ctx, cancel, cancelCtx
}

func chromeAnkerMerucari(ctx context.Context) error {
	var pageTitle string
	statusCode, err := navigateWithStatus(ctx, "https://jp.mercari.com/",
		chromedp.Title(&pageTitle),
	)
	if err != nil {
		return err
	}
	appLogger.Info("メルカリTOP", "画面遷移", statusLabel(statusCode))
	appLogger.Info("メルカリTOP", "ページタイトル", pageTitle)
	return nil
}

// NavigateToMercariMyPageListings は出品一覧ページに遷移して商品IDリストを返します。
func NavigateToMercariMyPageListings(ctx context.Context) ([]string, bool) {
	var itemIDs []string
	listingsURL := "https://jp.mercari.com/mypage/listings"

	var pageTitle, currentURL string
	var hrefs []map[string]string
	sel := `ul[data-testid="listed-item-list"] li a`

	statusCode, err := navigateWithStatus(ctx, listingsURL,
		chromedp.Location(&currentURL),
		chromedp.Title(&pageTitle),
		chromedp.WaitVisible(`ul[data-testid="listed-item-list"]`, chromedp.ByQuery),
	)
	if err != nil {
		appLogger.Error("出品一覧", "画面遷移", "失敗: "+err.Error())
		log.Fatalf("chromedp run error: %v", err)
	}
	appLogger.Info("出品一覧", "画面遷移", statusLabel(statusCode))
	appLogger.Info("出品一覧", "ページタイトル", pageTitle)

	if err := clickLoadMoreIfExists(ctx, 10, 800*time.Millisecond); err != nil {
		appLogger.Warn("出品一覧", "「もっと見る」クリック", "エラー: "+err.Error())
	}

	if err := chromedp.Run(ctx,
		chromedp.AttributesAll(sel, &hrefs, chromedp.ByQueryAll),
	); err != nil {
		appLogger.Error("出品一覧", "商品リンク取得", "失敗: "+err.Error())
		log.Fatalf("chromedp run error: %v", err)
	}

	if currentURL != listingsURL {
		appLogger.Warn("出品一覧", "URL確認", "想定外のURL: "+currentURL)
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

// IsLoginDomain は現在のドメインがログインページかどうかを確認します。
func IsLoginDomain(ctx context.Context) (bool, error) {
	var currentURL string
	if err := chromedp.Run(ctx, chromedp.Location(&currentURL)); err != nil {
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

	actions := []chromedp.Action{network.Enable(), chromedp.Navigate(targetURL)}
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

// newNetworkIdleWaiter はCDPネットワークイベントを監視するリスナーを登録し、
// 「全リクエストが完了してから idleFor 間何も来なければ nil を返す」wait 関数と
// リスナーを解放する stopListen 関数のペアを返します。
//
// 使い方:
//  1. クリック等の操作より前に呼び出してリスナーを起動する
//  2. 操作後に wait() を呼んで通信完了を待つ
//  3. 最後に stopListen() を呼ぶ（defer 推奨）
func newNetworkIdleWaiter(ctx context.Context, idleFor, timeout time.Duration) (wait func() error, stopListen func()) {
	var mu sync.Mutex
	pending := 0

	listenCtx, cancel := context.WithCancel(ctx)
	chromedp.ListenTarget(listenCtx, func(ev interface{}) {
		mu.Lock()
		defer mu.Unlock()
		switch ev.(type) {
		case *network.EventRequestWillBeSent:
			pending++
		case *network.EventLoadingFinished, *network.EventLoadingFailed:
			if pending > 0 {
				pending--
			}
		}
	})

	wait = func() error {
		deadline := time.Now().Add(timeout)
		var idleStart time.Time
		for {
			if time.Now().After(deadline) {
				return fmt.Errorf("通信完了タイムアウト (%v)", timeout)
			}
			mu.Lock()
			p := pending
			mu.Unlock()
			if p == 0 {
				if idleStart.IsZero() {
					idleStart = time.Now()
				} else if time.Since(idleStart) >= idleFor {
					return nil
				}
			} else {
				idleStart = time.Time{}
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
	stopListen = cancel
	return
}

// ─── 並列処理用タブ管理 ──────────────────────────────────────────────────────

// tabContext は並列処理用の追加タブとそのCDPコンテキストを管理します。
// プライマリタブ（run() で取得したもの）には使用しないでください。
type tabContext struct {
	ctx     context.Context
	cancel1 context.CancelFunc // allocator cancel
	cancel2 context.CancelFunc // context cancel
}

// newTabContext は新規タブを作成して tabContext を返します。
// 必ず Close() で解放してください。
func newTabContext() (*tabContext, error) {
	id := openNewTab()
	if id == "" {
		return nil, fmt.Errorf("新規タブ作成失敗")
	}
	ctx, c1, c2 := getContext(id)
	return &tabContext{ctx: ctx, cancel1: c1, cancel2: c2}, nil
}

// Close はCDPコンテキストを解放します。
func (t *tabContext) Close() {
	t.cancel2()
	t.cancel1()
}

func clickLoadMoreIfExists(ctx context.Context, maxClicks int, wait time.Duration) error {
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
		if err := chromedp.Run(ctx, chromedp.Evaluate(jsClick, &clicked)); err != nil {
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

