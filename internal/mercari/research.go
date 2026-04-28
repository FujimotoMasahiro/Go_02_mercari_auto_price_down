package mercari

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/chromedp/chromedp"

	"mercari-pricelower/internal/logger"
)

// Category はメルカリのカテゴリーを表します。
type Category struct {
	ID   string
	Name string
}

// CategoryList はメルカリの主要カテゴリー一覧です。
var CategoryList = []Category{
	{ID: "", Name: "すべて"},
	{ID: "5", Name: "レディース"},
	{ID: "6", Name: "メンズ"},
	{ID: "7", Name: "ベビー・キッズ"},
	{ID: "8", Name: "インテリア・住まい・小物"},
	{ID: "9", Name: "本・音楽・ゲーム"},
	{ID: "10", Name: "おもちゃ・ホビー・グッズ"},
	{ID: "11", Name: "コスメ・香水・美容"},
	{ID: "12", Name: "家電・スマホ・カメラ"},
	{ID: "13", Name: "スポーツ・レジャー"},
	{ID: "14", Name: "ハンドメイド"},
	{ID: "15", Name: "チケット"},
	{ID: "16", Name: "自動車・オートバイ"},
	{ID: "17", Name: "その他"},
}

// SoldItem は売り切れ商品の情報を表します。
type SoldItem struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Price int    `json:"price"`
	Page  int    `json:"page"`
}

// RunResearch は指定カテゴリーの売り切れ商品リサーチを実行します。
func RunResearch(categoryID, categoryName string, maxPages int) {
	var err error
	appLogger, err = logger.New()
	if err != nil {
		log.Fatalf("ロガー初期化失敗: %v", err)
	}
	defer appLogger.Close()

	appLogger.Separator()
	appLogger.Info("リサーチ開始", "カテゴリー", categoryName)
	appLogger.Info("リサーチ開始", "ページ数", fmt.Sprintf("%d", maxPages))

	if !isChromeRunning() {
		appLogger.Info("起動", "Chrome起動", "開始")
		cmd := launchChrome()
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

	var allItems []SoldItem
	for page := 1; page <= maxPages; page++ {
		searchURL := buildSearchURL(categoryID, page)
		appLogger.Info("リサーチ", fmt.Sprintf("ページ %d/%d", page, maxPages), searchURL)

		items, err := scrapeSoldItems(ctx, searchURL, page)
		if err != nil {
			appLogger.Warn("リサーチ", fmt.Sprintf("ページ %d スクレイピング失敗", page), err.Error())
			continue
		}
		appLogger.Info("リサーチ", fmt.Sprintf("ページ %d 取得完了", page), fmt.Sprintf("%d件", len(items)))
		allItems = append(allItems, items...)

		if page < maxPages {
			time.Sleep(1500 * time.Millisecond)
		}
	}

	if err := saveResearchCSV(categoryName, allItems); err != nil {
		appLogger.Error("リサーチ", "CSV保存", fmt.Sprintf("失敗: %v", err))
		return
	}

	appLogger.Info("リサーチ完了", "合計", fmt.Sprintf("%d件", len(allItems)))
	appLogger.Separator()
}

func buildSearchURL(categoryID string, page int) string {
	base := "https://jp.mercari.com/search?status=sold_out&sort=sold_desc"
	if categoryID != "" {
		base += "&categoryId=" + categoryID
	}
	if page > 1 {
		base += "&page=" + strconv.Itoa(page)
	}
	return base
}

// extractItemsJS はメルカリの検索結果ページから商品情報を取得するJavaScriptです。
const extractItemsJS = `(function() {
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
		const nameEl = link.querySelector('[data-testid="item-label"]') ||
		               link.querySelector('p[class*="name"]') ||
		               link.querySelector('p');
		if (nameEl) name = nameEl.textContent.trim();

		let priceText = '';
		const priceEl = link.querySelector('[data-testid="price"]') ||
		                link.querySelector('span[class*="price"]') ||
		                link.querySelector('[aria-label*="円"]');
		if (priceEl) priceText = priceEl.textContent.trim();

		let imgSrc = '';
		const imgEl = link.querySelector('img');
		if (imgEl) imgSrc = imgEl.src || imgEl.getAttribute('data-src') || '';

		if (id && (name || priceText)) {
			items.push({id, name, priceText, imgSrc});
		}
	});
	return JSON.stringify(items);
})()`

func scrapeSoldItems(ctx context.Context, searchURL string, page int) ([]SoldItem, error) {
	_, err := navigateWithStatus(ctx, searchURL,
		chromedp.WaitVisible(`a[href*="/item/"]`, chromedp.ByQuery),
	)
	if err != nil {
		return nil, fmt.Errorf("ページ読み込み失敗: %w", err)
	}

	time.Sleep(1500 * time.Millisecond)

	var resultJSON string
	if err := chromedp.Run(ctx, chromedp.Evaluate(extractItemsJS, &resultJSON)); err != nil {
		return nil, fmt.Errorf("スクレイピング失敗: %w", err)
	}

	type rawItem struct {
		ID        string `json:"id"`
		Name      string `json:"name"`
		PriceText string `json:"priceText"`
	}
	var rawItems []rawItem
	if err := json.Unmarshal([]byte(resultJSON), &rawItems); err != nil {
		return nil, fmt.Errorf("JSONパース失敗: %w", err)
	}

	var items []SoldItem
	for _, r := range rawItems {
		price := parsePriceText(r.PriceText)
		items = append(items, SoldItem{
			ID:    r.ID,
			Name:  r.Name,
			Price: price,
			Page:  page,
		})
	}
	return items, nil
}

func parsePriceText(s string) int {
	s = strings.ReplaceAll(s, ",", "")
	s = strings.ReplaceAll(s, "円", "")
	s = strings.ReplaceAll(s, "¥", "")
	s = strings.ReplaceAll(s, "￥", "")
	s = strings.TrimSpace(s)
	n, _ := strconv.Atoi(s)
	return n
}

func saveResearchCSV(categoryName string, items []SoldItem) error {
	dir := "research"
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	fname := time.Now().Format("20060102150405") + "_" + sanitizeFilename(categoryName) + ".csv"
	path := filepath.Join(dir, fname)
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()

	w := csv.NewWriter(f)
	defer w.Flush()
	w.Write([]string{"商品ID", "商品名", "価格(円)", "ページ"})
	for _, item := range items {
		w.Write([]string{item.ID, item.Name, strconv.Itoa(item.Price), strconv.Itoa(item.Page)})
	}
	appLogger.Info("リサーチ", "CSV保存完了", path)
	return nil
}

func sanitizeFilename(s string) string {
	r := strings.NewReplacer(
		"/", "_", "\\", "_", ":", "_", "*", "_",
		"?", "_", `"`, "_", "<", "_", ">", "_", "|", "_",
		" ", "_", "　", "_",
	)
	return r.Replace(s)
}
