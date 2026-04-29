package mercari

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/chromedp"

	"mercari-pricelower/internal/logger"
)

// ResearchImgDir はリサーチ結果のサムネイル画像を保存するディレクトリ名です。
const ResearchImgDir = "research_img"

// researchDetailConcurrency は商品詳細ページ取得時に使用する並列タブ数です。
// 3タブ並列で処理速度を高めつつ、メルカリへの過剰アクセスを抑制します。
const researchDetailConcurrency = 3

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
	ID      string `json:"id"`
	Name    string `json:"name"`
	Price   int    `json:"price"`
	SoldIn  string `json:"sold_in,omitempty"` // 例: "6分" / "2時間" / "3日"
	ImgPath string `json:"img_path,omitempty"`
}

// ─── cdpImageTracker ────────────────────────────────────────────────────────

// cdpReqState はCDPが捕捉した単一リクエストの状態を保持します。
type cdpReqState struct {
	status   int
	finished bool
}

// cdpImageTracker はCDPのネットワークイベントを監視し、
// ページ上の画像データをキャプチャして保存するオブジェクトです。
// ページナビゲーション前に起動することで、ロードされた画像を取りこぼしません。
type cdpImageTracker struct {
	mu       sync.Mutex
	byID     map[network.RequestID]*cdpReqState
	byURL    map[string]network.RequestID
	cancelFn context.CancelFunc
}

// newCDPImageTracker は ctx を監視するトラッカーを起動して返します。
// 呼び出し元は必ず Stop() をデファーしてリスナーを解放してください。
func newCDPImageTracker(ctx context.Context) *cdpImageTracker {
	t := &cdpImageTracker{
		byID:  make(map[network.RequestID]*cdpReqState),
		byURL: make(map[string]network.RequestID),
	}
	listenCtx, cancel := context.WithCancel(ctx)
	t.cancelFn = cancel

	chromedp.ListenTarget(listenCtx, func(ev interface{}) {
		switch e := ev.(type) {
		case *network.EventRequestWillBeSent:
			t.mu.Lock()
			t.byID[e.RequestID] = &cdpReqState{}
			t.byURL[e.Request.URL] = e.RequestID
			t.mu.Unlock()
		case *network.EventResponseReceived:
			t.mu.Lock()
			if s, ok := t.byID[e.RequestID]; ok {
				s.status = int(e.Response.Status)
			}
			t.mu.Unlock()
		case *network.EventLoadingFinished:
			t.mu.Lock()
			if s, ok := t.byID[e.RequestID]; ok {
				s.finished = true
			}
			t.mu.Unlock()
		}
	})
	return t
}

// Stop はCDPリスナーを解放します。
func (t *cdpImageTracker) Stop() { t.cancelFn() }

// SaveImage は imgSrc の画像を imgDir/itemID.ext に保存し、Web配信用パスを返します。
// 保存済みの場合はそのパスを返します。CDPで未キャプチャの場合は空文字列を返します。
func (t *cdpImageTracker) SaveImage(ctx context.Context, imgDir, itemID, imgSrc string) string {
	if imgSrc == "" {
		return ""
	}
	ext := imageExt(imgSrc)
	localPath := filepath.Join(imgDir, itemID+ext)

	// 既存ファイルがある場合はWebパスのみ返す
	if _, err := os.Stat(localPath); err == nil {
		return "/" + strings.ReplaceAll(localPath, string(filepath.Separator), "/")
	}

	t.mu.Lock()
	reqID, found := t.byURL[imgSrc]
	var state *cdpReqState
	if found {
		state = t.byID[reqID]
	}
	t.mu.Unlock()

	if !found || state == nil || state.status != 200 || !state.finished {
		return ""
	}

	var body []byte
	if err := chromedp.Run(ctx, chromedp.ActionFunc(func(ctx context.Context) error {
		var e error
		body, e = network.GetResponseBody(reqID).Do(ctx)
		return e
	})); err != nil {
		return ""
	}
	if err := os.WriteFile(localPath, body, 0644); err != nil {
		return ""
	}
	return "/" + strings.ReplaceAll(localPath, string(filepath.Separator), "/")
}

// ─── ResearchRunner ─────────────────────────────────────────────────────────

// ResearchRunner はメルカリ売れ筋リサーチの全体フローを管理するオブジェクトです。
type ResearchRunner struct {
	ctx          context.Context
	logger       *logger.AppLogger
	categoryID   string
	categoryName string
	maxPages     int
	soldInCache  map[string]string // itemID → 売れた時間（過去データキャッシュ）
}

// newResearchRunner は ResearchRunner を初期化して返します。
// 過去のリサーチCSVから売れた時間キャッシュを自動ロードします。
func newResearchRunner(ctx context.Context, log *logger.AppLogger,
	categoryID, categoryName string, maxPages int) *ResearchRunner {
	cache := loadResearchCache()
	log.Info("キャッシュ読込", "件数", fmt.Sprintf("%d件", len(cache)))
	return &ResearchRunner{
		ctx:          ctx,
		logger:       log,
		categoryID:   categoryID,
		categoryName: categoryName,
		maxPages:     maxPages,
		soldInCache:  cache,
	}
}

// loadResearchCache は過去のリサーチCSVを全て読み込み、
// 商品ID → 売れた時間 のキャッシュマップを返します。
// 同じIDが複数ファイルにある場合は最新ファイル優先（ファイル名降順）です。
func loadResearchCache() map[string]string {
	cache := make(map[string]string)
	dir := "research"
	entries, err := os.ReadDir(dir)
	if err != nil {
		return cache
	}

	// ファイル名降順（新しいものが先）でソートして読み込む
	var csvFiles []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".csv") {
			csvFiles = append(csvFiles, e.Name())
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(csvFiles)))

	for _, fname := range csvFiles {
		f, err := os.Open(filepath.Join(dir, fname))
		if err != nil {
			continue
		}
		records, err := csv.NewReader(f).ReadAll()
		f.Close()
		if err != nil {
			continue
		}
		for i, row := range records {
			if i == 0 || len(row) < 4 {
				continue
			}
			id, soldIn := strings.TrimSpace(row[0]), strings.TrimSpace(row[3])
			// 最新ファイル優先：既にエントリがある場合は上書きしない
			if id != "" && soldIn != "" {
				if _, exists := cache[id]; !exists {
					cache[id] = soldIn
				}
			}
		}
	}
	return cache
}

// Run はリサーチを実行し、全売り切れ商品一覧を返します。
// フェーズ1: 検索結果ページを全ページ巡回して商品一覧を取得
// フェーズ2: 各商品詳細ページを確認して売れた時間を取得
func (r *ResearchRunner) Run() ([]SoldItem, error) {
	// ── フェーズ1: 検索結果スクレイピング ────────────────────────────────────
	var allItems []SoldItem
	for page := 1; page <= r.maxPages; page++ {
		searchURL := buildSearchURL(r.categoryID, page)
		r.logger.Info("リサーチ", fmt.Sprintf("ページ %d/%d", page, r.maxPages), searchURL)

		items, err := r.scrapePage(searchURL, page)
		if err != nil {
			r.logger.Warn("リサーチ", fmt.Sprintf("ページ %d スクレイピング失敗", page), err.Error())
			continue
		}
		r.logger.Info("リサーチ", fmt.Sprintf("ページ %d 取得完了", page), fmt.Sprintf("%d件", len(items)))
		allItems = append(allItems, items...)

		if page < r.maxPages {
			time.Sleep(1500 * time.Millisecond)
		}
	}

	// ── フェーズ2: 商品詳細ページで売れた時間を取得 ──────────────────────────
	allItems = r.scrapeDetailPages(allItems)

	return allItems, nil
}

// scrapeDetailPages は各商品詳細ページを巡回し、
// 「XX分/時間/日で売れた」テキストを取得して SoldIn フィールドに設定します。
// 過去CSVキャッシュにヒットした商品はページ遷移をスキップします。
// researchDetailConcurrency 本のタブを並列に使って処理速度を向上させます。
func (r *ResearchRunner) scrapeDetailPages(items []SoldItem) []SoldItem {
	total := len(items)

	// ── フェーズ1: キャッシュヒット分を先に処理 ─────────────────────────────
	type pendingJob struct {
		itemIdx int
		id      string
	}
	var pending []pendingJob
	cacheHit := 0

	for i := range items {
		if cached, ok := r.soldInCache[items[i].ID]; ok && cached != "" {
			items[i].SoldIn = cached
			cacheHit++
			r.logger.Info("詳細取得", "(キャッシュ) "+items[i].ID, "→ "+cached)
		} else {
			pending = append(pending, pendingJob{i, items[i].ID})
		}
	}

	if len(pending) == 0 {
		r.logger.Info("詳細取得", "完了",
			fmt.Sprintf("全%d件キャッシュヒット（スクレイピング不要）", cacheHit))
		return items
	}

	// ── フェーズ2: 並列スクレイピング ────────────────────────────────────────
	concurrency := researchDetailConcurrency
	if len(pending) < concurrency {
		concurrency = len(pending)
	}

	r.logger.Info("詳細取得", "開始",
		fmt.Sprintf("スクレイピング%d件（並列%dタブ / キャッシュ%d件スキップ）",
			len(pending), concurrency, cacheHit))

	// タブ作成: プライマリタブ(r.ctx)をworker 0として使用し、追加タブを作成する
	type worker struct {
		ctx    context.Context
		tabCtx *tabContext // nil = プライマリタブ（Close不要）
	}
	workers := []worker{{ctx: r.ctx}}

	for i := 1; i < concurrency; i++ {
		t, err := newTabContext()
		if err != nil {
			r.logger.Warn("詳細取得", fmt.Sprintf("追加タブ%d作成失敗", i), err.Error())
			break
		}
		// メルカリTOPに遷移してログイン状態（Cookie）を引き継ぐ
		if navErr := chromedp.Run(t.ctx, chromedp.Navigate("https://jp.mercari.com/")); navErr != nil {
			r.logger.Warn("詳細取得", fmt.Sprintf("追加タブ%d初期化失敗", i), navErr.Error())
			t.Close()
			break
		}
		workers = append(workers, worker{ctx: t.ctx, tabCtx: t})
	}
	defer func() {
		for _, w := range workers {
			if w.tabCtx != nil {
				w.tabCtx.Close()
			}
		}
	}()

	r.logger.Info("詳細取得", "並列処理設定",
		fmt.Sprintf("%dタブで並列処理", len(workers)))

	// ── ワーカープール ────────────────────────────────────────────────────────
	type job struct {
		itemIdx int
		id      string
	}
	type result struct {
		itemIdx int
		soldIn  string
	}

	jobCh    := make(chan job, len(pending))
	resultCh := make(chan result, len(pending))

	var (
		counterMu sync.Mutex
		doneCount int
	)

	var wg sync.WaitGroup
	for wi, w := range workers {
		wg.Add(1)
		go func(workerID int, workerCtx context.Context) {
			defer wg.Done()
			for j := range jobCh {
				soldIn := scrapeSoldTimeCtx(workerCtx, j.id)

				counterMu.Lock()
				doneCount++
				done := doneCount
				counterMu.Unlock()

				label := "→ 取得不可"
				if soldIn != "" {
					label = "→ " + soldIn
				}
				r.logger.Info("詳細取得",
					fmt.Sprintf("(%d/%d)[w%d]", done, len(pending), workerID+1),
					j.id+" "+label)

				resultCh <- result{j.itemIdx, soldIn}
			}
		}(wi, w.ctx)
	}

	for _, p := range pending {
		jobCh <- job{p.itemIdx, p.id}
	}
	close(jobCh)

	wg.Wait()
	close(resultCh)

	for res := range resultCh {
		items[res.itemIdx].SoldIn = res.soldIn
	}

	r.logger.Info("詳細取得", "完了",
		fmt.Sprintf("%d件処理（スクレイピング%d件 / キャッシュ%d件）",
			total, len(pending), cacheHit))
	return items
}

// scrapeSoldTimeCtx は商品詳細ページから「XX分/時間/日で売れた」テキストを抽出します。
// 売れた時間バナーはReactの非同期コンポーネントのため、最大15秒間ポーリングして待機します。
// 「約N分」「N時間以内」など表記ゆれにも対応しています。
// 情報が見つからない場合や読み込みに失敗した場合は空文字列を返します。
func scrapeSoldTimeCtx(ctx context.Context, itemID string) string {
	// 「約」「以内」などの表記ゆれを正規表現で吸収する。
	// innerText と innerHTML の両方を検索して取りこぼしを防ぐ。
	const soldTimeJS = `(function() {
		const text = (document.body.innerText || '') + '\n' + (document.body.innerHTML || '');
		const m = text.match(/約?(\d+)(分|時間|日|週間|ヶ月|か月)(以内)?で売れた/);
		return m ? m[1] + m[2] : '';
	})()`

	detailURL := "https://jp.mercari.com/item/" + itemID

	// 全体タイムアウト: ナビゲーション(最大10s) + ポーリング(15s) = 25s
	pageCtx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()

	if _, err := navigateWithStatus(pageCtx, detailURL); err != nil {
		return ""
	}

	// スクロールして遅延ロードコンポーネント（レコメンドバナー等）を表示させる
	chromedp.Run(pageCtx, chromedp.Evaluate(`window.scrollTo(0, 400)`, nil))
	time.Sleep(300 * time.Millisecond)

	// 売れた時間バナーはレコメンドAPIの応答後に後からレンダリングされる。
	// 500msごとにポーリングして最大15秒待機（時間・日単位は応答が遅い場合がある）。
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		var soldIn string
		if err := chromedp.Run(pageCtx, chromedp.Evaluate(soldTimeJS, &soldIn)); err == nil && soldIn != "" {
			return soldIn
		}
		time.Sleep(500 * time.Millisecond)
	}

	return ""
}

// scrapePage は1ページ分の売り切れ商品をスクレイピングし、
// CDPで取得した画像もあわせて保存します。
func (r *ResearchRunner) scrapePage(searchURL string, page int) ([]SoldItem, error) {
	// ナビゲーション前にCDPトラッカーを起動してリクエストを取りこぼさない
	tracker := newCDPImageTracker(r.ctx)
	defer tracker.Stop()

	if _, err := navigateWithStatus(r.ctx, searchURL,
		chromedp.WaitVisible(`a[href*="/item/"]`, chromedp.ByQuery),
	); err != nil {
		return nil, fmt.Errorf("ページ読み込み失敗: %w", err)
	}

	// ページ全体をスクロールして遅延ロード商品・画像を全て表示させる
	r.scrollToLoadAll()

	rawItems, err := r.extractRawItems()
	if err != nil {
		return nil, err
	}

	if err := os.MkdirAll(ResearchImgDir, 0755); err != nil {
		r.logger.Warn("リサーチ", "画像ディレクトリ作成失敗", err.Error())
	}

	items := make([]SoldItem, 0, len(rawItems))
	for _, raw := range rawItems {
		imgPath := tracker.SaveImage(r.ctx, ResearchImgDir, raw.ID, raw.ImgSrc)
		items = append(items, SoldItem{
			ID:      raw.ID,
			Name:    raw.Name,
			Price:   parsePriceText(raw.PriceText),
			ImgPath: imgPath,
		})
	}
	return items, nil
}

// scrollToLoadAll はページを上から下へゆっくりスクロールし、
// 遅延ロードされる全商品カードと画像の読み込み完了を待機します。
func (r *ResearchRunner) scrollToLoadAll() {
	var bodyHeight, viewHeight float64
	chromedp.Run(r.ctx,
		chromedp.Evaluate(`document.body.scrollHeight`, &bodyHeight),
		chromedp.Evaluate(`window.innerHeight`, &viewHeight),
	)
	if viewHeight <= 0 {
		viewHeight = 800
	}

	// ビューポートの70%ずつスクロールして遅延ロードを確実にトリガーする
	step := viewHeight * 0.7
	for y := 0.0; y <= bodyHeight+step; y += step {
		chromedp.Run(r.ctx, chromedp.Evaluate(fmt.Sprintf(`window.scrollTo(0, %d)`, int(y)), nil))
		time.Sleep(350 * time.Millisecond)
	}

	// 全商品画像の読み込み完了を最大15秒待機
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		var allComplete bool
		chromedp.Run(r.ctx, chromedp.Evaluate(`
			(function(){
				const imgs = Array.from(document.querySelectorAll('a[href*="/item/"] img'));
				return imgs.length > 0 && imgs.every(img => img.complete && img.naturalWidth > 0);
			})()
		`, &allComplete))
		if allComplete {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// rawSoldItem はJSから取得した変換前の商品データです。
type rawSoldItem struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	PriceText string `json:"priceText"`
	ImgSrc    string `json:"imgSrc"`
}

// extractRawItems はDOM上の全商品情報をJS経由で一括取得します。
func (r *ResearchRunner) extractRawItems() ([]rawSoldItem, error) {
	var resultJSON string
	if err := chromedp.Run(r.ctx, chromedp.Evaluate(extractItemsJS, &resultJSON)); err != nil {
		return nil, fmt.Errorf("スクレイピング失敗: %w", err)
	}
	var items []rawSoldItem
	if err := json.Unmarshal([]byte(resultJSON), &items); err != nil {
		return nil, fmt.Errorf("JSONパース失敗: %w", err)
	}
	return items, nil
}

// ─── 公開エントリポイント ────────────────────────────────────────────────────

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

	runner := newResearchRunner(ctx, appLogger, categoryID, categoryName, maxPages)
	allItems, err := runner.Run()
	if err != nil {
		appLogger.Error("リサーチ", "実行失敗", err.Error())
		return
	}

	if err := saveResearchCSV(categoryName, allItems); err != nil {
		appLogger.Error("リサーチ", "CSV保存", fmt.Sprintf("失敗: %v", err))
		return
	}

	appLogger.Info("リサーチ完了", "合計", fmt.Sprintf("%d件", len(allItems)))
	appLogger.Separator()
}

// ─── ユーティリティ ─────────────────────────────────────────────────────────

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

// extractItemsJS はメルカリ検索結果ページから全商品情報を取得するJavaScriptです。
//
// 商品名の取得優先順位:
//  1. data-testid="item-label"  (マイページ出品一覧で使われる属性)
//  2. data-testid="item-name"   (別バージョンの属性)
//  3. aria-label                (アクセシビリティ属性)
//  4. img[alt]                  (検索結果ページでは画像altに商品名が入る)
//  5. 内包するp/spanのテキスト  (最終フォールバック)
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

		// ── 商品名抽出 ──────────────────────────────────────────────────────────
		let name = '';
		// 1. data-testid 属性（マイページ出品一覧ページで使用）
		const labelEl = link.querySelector('[data-testid="item-label"]') ||
		                link.querySelector('[data-testid="item-name"]');
		if (labelEl) name = labelEl.textContent.trim();

		// 2. aria-label（リンク自体またはカード要素に設定されている場合）
		if (!name) {
			const al = link.getAttribute('aria-label') || '';
			if (al && !al.includes('円')) name = al.trim();
		}

		// 3. img[alt]（検索結果ページでは画像のalt属性に商品名が入ることが多い）
		const imgEl = link.querySelector('img');
		if (!name && imgEl) {
			const alt = imgEl.getAttribute('alt') || '';
			if (alt) name = alt.trim();
		}

		// 4. テキストを持つ最初のp/span要素（最終フォールバック）
		if (!name) {
			const textEl = link.querySelector('p') || link.querySelector('span');
			if (textEl) name = textEl.textContent.trim();
		}

		// ── 価格抽出 ────────────────────────────────────────────────────────────
		let priceText = '';
		const priceEl = link.querySelector('[data-testid="price"]') ||
		                link.querySelector('span[class*="price"]') ||
		                link.querySelector('[aria-label*="円"]');
		if (priceEl) priceText = priceEl.textContent.trim();

		// ── 画像URL ─────────────────────────────────────────────────────────────
		let imgSrc = '';
		if (imgEl) imgSrc = imgEl.src || imgEl.getAttribute('data-src') || '';

		if (id && (name || priceText)) {
			items.push({id, name, priceText, imgSrc});
		}
	});
	return JSON.stringify(items);
})()`

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
	w.Write([]string{"商品ID", "商品名", "価格(円)", "売れた時間"})
	for _, item := range items {
		w.Write([]string{item.ID, item.Name, strconv.Itoa(item.Price), item.SoldIn})
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
