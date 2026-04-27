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
	"os/exec"
	"strings"
	"time"

	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/cdproto/target"
	"github.com/chromedp/chromedp"

	"mercari-pricelower/internal/config"
)

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
	if !config.ViewFlg {
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

// GetNewTabID はChrome DevTools から操作対象タブのIDを取得します。
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

