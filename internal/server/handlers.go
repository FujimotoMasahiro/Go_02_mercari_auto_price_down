package server

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"mercari-pricelower/internal/config"
	"mercari-pricelower/internal/costs"
	"mercari-pricelower/internal/db"
	"mercari-pricelower/internal/history"
)

func openBrowser(url string) {
	time.Sleep(500 * time.Millisecond)
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		cmd = exec.Command("cmd", "/c", "start", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	if err := cmd.Start(); err != nil {
		fmt.Printf("ブラウザ起動失敗: %v\n", err)
	}
}

func handleIndex(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(indexHTML)
}

func handleProducts(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(productsHTML)
}

type historyRunInfo struct {
	Timestamp time.Time `json:"timestamp"`
}

type historyCell struct {
	Price    *int   `json:"price"`    // 値引き後価格（値引き成功時のみ）
	OldPrice *int   `json:"oldPrice"` // 値引き前価格 or スキップ時の価格
	Reason   string `json:"reason"`   // スキップ理由（""=値引き成功 or データなし）
}

type historyProductRow struct {
	ItemID    string        `json:"itemId"`
	ItemName  string        `json:"itemName"`
	ImagePath string        `json:"imagePath,omitempty"`
	Cells     []historyCell `json:"cells"`
}

func handleHistory(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(productsHTML)
}

func handleHistoryData(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	entries, err := history.Load()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	const maxRuns = 5
	if len(entries) > maxRuns {
		entries = entries[:maxRuns]
	}

	if len(entries) == 0 {
		json.NewEncoder(w).Encode(map[string]interface{}{
			"runs":     []historyRunInfo{},
			"products": []historyProductRow{},
		})
		return
	}

	// 実行回ごとの値引き成功マップ・スキップマップ
	type discountInfo struct{ oldPrice, newPrice int }
	type skipInfo struct{ price int; reason string }
	runDiscounts := make([]map[string]discountInfo, len(entries))
	runSkipped := make([]map[string]skipInfo, len(entries))
	var productOrder []string
	productNames := map[string]string{}
	for i, entry := range entries {
		runDiscounts[i] = map[string]discountInfo{}
		runSkipped[i] = map[string]skipInfo{}
		for _, p := range entry.Products {
			runDiscounts[i][p.ItemID] = discountInfo{oldPrice: p.OldPrice, newPrice: p.NewPrice}
			if _, exists := productNames[p.ItemID]; !exists {
				productOrder = append(productOrder, p.ItemID)
				productNames[p.ItemID] = p.ItemName
			}
		}
		for _, s := range entry.Skipped {
			runSkipped[i][s.ItemID] = skipInfo{price: s.Price, reason: s.Reason}
			if _, exists := productNames[s.ItemID]; !exists {
				productOrder = append(productOrder, s.ItemID)
				productNames[s.ItemID] = s.ItemName
			}
		}
	}

	runs := make([]historyRunInfo, len(entries))
	for i, e := range entries {
		runs[i] = historyRunInfo{Timestamp: e.Timestamp}
	}

	imgDir := filepath.Join(projectRoot(), config.DirImg)
	products := make([]historyProductRow, 0, len(productOrder))
	for _, id := range productOrder {
		cells := make([]historyCell, len(entries))
		for i := range entries {
			if d, ok := runDiscounts[i][id]; ok {
				oldP := d.oldPrice
				newP := d.newPrice
				cells[i] = historyCell{Price: &newP, OldPrice: &oldP}
			} else if s, ok := runSkipped[i][id]; ok {
				cell := historyCell{Reason: s.reason}
				if s.price > 0 {
					p := s.price
					cell.OldPrice = &p
				}
				cells[i] = cell
			}
		}
		row := historyProductRow{
			ItemID:   id,
			ItemName: productNames[id],
			Cells:    cells,
		}
		if matches, _ := filepath.Glob(filepath.Join(imgDir, id+".*")); len(matches) > 0 {
			row.ImagePath = "/img/" + filepath.Base(matches[0])
		}
		products = append(products, row)
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"runs":     runs,
		"products": products,
	})
}

func handleCSVData(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	csvDir := filepath.Join(projectRoot(), config.DirCSV)
	entries, err := os.ReadDir(csvDir)
	if err != nil {
		json.NewEncoder(w).Encode(map[string]interface{}{"items": []ProductItem{}, "file": ""})
		return
	}

	var csvFiles []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".csv") {
			csvFiles = append(csvFiles, e.Name())
		}
	}
	if len(csvFiles) == 0 {
		json.NewEncoder(w).Encode(map[string]interface{}{"items": []ProductItem{}, "file": ""})
		return
	}
	sort.Sort(sort.Reverse(sort.StringSlice(csvFiles)))
	latestFile := csvFiles[0]

	f, err := os.Open(filepath.Join(csvDir, latestFile))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer f.Close()

	records, err := csv.NewReader(f).ReadAll()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	imgDir := filepath.Join(projectRoot(), config.DirImg)
	var items []ProductItem
	for i, row := range records {
		if i == 0 {
			continue
		}
		if len(row) < 3 {
			continue
		}
		item := ProductItem{ID: row[0], Name: row[1], Price: row[2]}
		if matches, _ := filepath.Glob(filepath.Join(imgDir, row[0]+".*")); len(matches) > 0 {
			item.ImagePath = "/img/" + filepath.Base(matches[0])
		}
		items = append(items, item)
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"items": items,
		"file":  latestFile,
	})
}

func handleResearch(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(researchHTML)
}

func handleResearchData(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	type researchItem struct {
		ID        string `json:"id"`
		Name      string `json:"name"`
		Price     int    `json:"price"`
		SoldIn    string `json:"soldIn,omitempty"`
		ImagePath string `json:"imagePath,omitempty"`
		Category  string `json:"category,omitempty"`
	}

	researchImgDir := filepath.Join(projectRoot(), config.DirResearchImg)

	attachImage := func(itemID string) string {
		if matches, _ := filepath.Glob(filepath.Join(researchImgDir, itemID+".*")); len(matches) > 0 {
			return "/research_img/" + filepath.Base(matches[0])
		}
		return ""
	}

	// ── DB優先: 接続済みであれば最新セッションから取得 ──────────────
	if db.IsOpen() {
		sessionID, createdAt := db.GetLatestSession()
		if sessionID > 0 {
			dbItems, err := db.GetSessionItems(sessionID)
			if err == nil {
				items := make([]researchItem, 0, len(dbItems))
				for _, it := range dbItems {
					items = append(items, researchItem{
						ID:        it.ItemID,
						Name:      it.Name,
						Price:     it.Price,
						SoldIn:    it.SoldIn,
						Category:  it.Category,
						ImagePath: attachImage(it.ItemID),
					})
				}
				label := fmt.Sprintf("DB: session #%d (%s)",
					sessionID, createdAt.In(time.FixedZone("JST", 9*60*60)).Format("2006/01/02 15:04"))
				json.NewEncoder(w).Encode(map[string]interface{}{
					"items": items,
					"file":  label,
				})
				return
			}
		}
	}

	// ── フォールバック: CSVファイルから読み込み ─────────────────────
	researchDir := filepath.Join(projectRoot(), config.DirResearch)
	entries, err := os.ReadDir(researchDir)
	if err != nil {
		json.NewEncoder(w).Encode(map[string]interface{}{"items": []researchItem{}, "file": ""})
		return
	}
	var csvFiles []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".csv") {
			csvFiles = append(csvFiles, e.Name())
		}
	}
	if len(csvFiles) == 0 {
		json.NewEncoder(w).Encode(map[string]interface{}{"items": []researchItem{}, "file": ""})
		return
	}
	sort.Sort(sort.Reverse(sort.StringSlice(csvFiles)))
	latestFile := csvFiles[0]

	f, err := os.Open(filepath.Join(researchDir, latestFile))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer f.Close()

	records, err := csv.NewReader(f).ReadAll()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	var items []researchItem
	for i, row := range records {
		if i == 0 {
			continue
		}
		if len(row) < 3 {
			continue
		}
		price, _ := strconv.Atoi(row[2])
		soldIn := ""
		if len(row) >= 4 {
			soldIn = row[3]
		}
		category := ""
		if len(row) >= 5 {
			category = row[4]
		}
		item := researchItem{
			ID:        row[0],
			Name:      row[1],
			Price:     price,
			SoldIn:    soldIn,
			Category:  category,
			ImagePath: attachImage(row[0]),
		}
		items = append(items, item)
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"items": items,
		"file":  latestFile,
	})
}

// handleResearchLive は research/live.json をそのまま配信します。
// フロントエンドが2秒ごとにポーリングしてリアルタイム表示に使用します。
func handleResearchLive(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-cache")
	liveFile := filepath.Join(projectRoot(), config.DirResearch, "live.json")
	data, err := os.ReadFile(liveFile)
	if err != nil {
		json.NewEncoder(w).Encode(map[string]interface{}{"items": []interface{}{}})
		return
	}
	w.Write(data)
}

func handleRunResearch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var body struct {
		CategoryID    string   `json:"categoryId"`
		CategoryName  string   `json:"categoryName"`
		Pages         int      `json:"pages"`
		Keyword       string   `json:"keyword"`
		PriceMin      int      `json:"priceMin"`
		PriceMax      int      `json:"priceMax"`
		Conditions    []string `json:"conditions"`
		ShippingPayer string   `json:"shippingPayer"`
	}
	if r.Header.Get("Content-Type") == "application/json" {
		json.NewDecoder(r.Body).Decode(&body)
	}
	if body.Pages < 1 {
		body.Pages = 5
	}
	if body.Pages > 10 {
		body.Pages = 10
	}
	if body.CategoryName == "" {
		body.CategoryName = "すべて"
	}

	// リサーチ条件を config.json に保存して次回復元できるようにする
	newCfg := config.Cfg
	newCfg.ResearchConditions = &config.ResearchConditions{
		CategoryID:    body.CategoryID,
		CategoryName:  body.CategoryName,
		Pages:         body.Pages,
		Keyword:       body.Keyword,
		PriceMin:      body.PriceMin,
		PriceMax:      body.PriceMax,
		Conditions:    body.Conditions,
		ShippingPayer: body.ShippingPayer,
	}
	_ = config.Save(newCfg)

	extraArgs := []string{
		"-category-id=" + body.CategoryID,
		"-category=" + body.CategoryName,
		fmt.Sprintf("-pages=%d", body.Pages),
	}
	if body.Keyword != "" {
		extraArgs = append(extraArgs, "-keyword="+body.Keyword)
	}
	if body.PriceMin > 0 {
		extraArgs = append(extraArgs, fmt.Sprintf("-price-min=%d", body.PriceMin))
	}
	if body.PriceMax > 0 {
		extraArgs = append(extraArgs, fmt.Sprintf("-price-max=%d", body.PriceMax))
	}
	if len(body.Conditions) > 0 {
		extraArgs = append(extraArgs, "-conditions="+strings.Join(body.Conditions, ","))
	}
	if body.ShippingPayer != "" {
		extraArgs = append(extraArgs, "-shipping-payer="+body.ShippingPayer)
	}
	startResearch(w, findResearchBinary, extraArgs...)
}

func handleRun(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var body struct {
		ExcludedIDs []string `json:"excludedIds"`
	}
	if r.Header.Get("Content-Type") == "application/json" {
		json.NewDecoder(r.Body).Decode(&body)
	}

	var extraArgs []string
	if len(body.ExcludedIDs) > 0 {
		extraArgs = []string{"-exclude", strings.Join(body.ExcludedIDs, ",")}
	}

	startBinary(w, findBinary, extraArgs...)
}

func handleCreateCSV(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	startBinary(w, findCSVBinary)
}

func handleStop(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	mu.Lock()
	running := isRunning
	mu.Unlock()
	if !running {
		json.NewEncoder(w).Encode(map[string]string{"status": "not_running"})
		return
	}
	json.NewEncoder(w).Encode(map[string]string{"status": "stop_requested"})
}

func handleStatus(w http.ResponseWriter, r *http.Request) {
	mu.Lock()
	running := isRunning
	output := outputBuf.String()
	mu.Unlock()

	resMu.Lock()
	resRun := resRunning
	resMu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"running":         running,
		"output":          output,
		"researchRunning": resRun,
	})
}

func handleLogs(w http.ResponseWriter, r *http.Request) {
	dirs := findLogDirs()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{"dirs": dirs})
}

func handleLogFile(w http.ResponseWriter, r *http.Request) {
	rel := r.URL.Query().Get("path")
	if rel == "" {
		http.Error(w, "path required", http.StatusBadRequest)
		return
	}

	abs, err := filepath.Abs(filepath.Join(projectRoot(), rel))
	if err != nil {
		http.Error(w, "invalid path", http.StatusBadRequest)
		return
	}
	allowed := false
	for _, d := range findLogDirs() {
		dirAbs, _ := filepath.Abs(filepath.Join(projectRoot(), d.Name))
		if strings.HasPrefix(abs, dirAbs+string(filepath.Separator)) {
			allowed = true
			break
		}
	}
	if !allowed {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	content, err := os.ReadFile(abs)
	if err != nil {
		http.Error(w, "cannot read file: "+err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{
		"filename": filepath.Base(abs),
		"content":  string(content),
	})
}

// ── 設定画面 ────────────────────────────────────────────────────────────────

func handleSettings(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(settingsHTML)
}

// handleConfig は GET で現在設定を返し、POST で config.json を更新します。
func handleConfig(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(config.Cfg)

	case http.MethodPost:
		var newCfg config.AppConfig
		if err := json.NewDecoder(r.Body).Decode(&newCfg); err != nil {
			http.Error(w, `{"error":"JSONパース失敗: `+err.Error()+`"}`, http.StatusBadRequest)
			return
		}
		// バリデーション
		if newCfg.MinPrice < 0 {
			newCfg.MinPrice = 0
		}
		if newCfg.PriceDecreaseAmount < 0 {
			newCfg.PriceDecreaseAmount = 0
		}
		if err := config.Save(newCfg); err != nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(map[string]string{"error": "保存失敗: " + err.Error()})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"status": "ok"})

	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

func handleEvents(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	ch := make(chan string, 200)
	clientsMu.Lock()
	clients = append(clients, ch)
	clientsMu.Unlock()

	defer func() {
		clientsMu.Lock()
		for i, c := range clients {
			if c == ch {
				clients = append(clients[:i], clients[i+1:]...)
				break
			}
		}
		clientsMu.Unlock()
		close(ch)
	}()

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "Streaming not supported", http.StatusInternalServerError)
		return
	}

	mu.Lock()
	current := outputBuf.String()
	mu.Unlock()
	for _, line := range strings.Split(current, "\n") {
		if line != "" {
			fmt.Fprintf(w, "data: %s\n\n", escapeSSE(line))
		}
	}
	flusher.Flush()

	ticker := time.NewTicker(25 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case msg, ok := <-ch:
			if !ok {
				return
			}
			fmt.Fprintf(w, "data: %s\n\n", escapeSSE(msg))
			flusher.Flush()
		case <-ticker.C:
			fmt.Fprintf(w, ": ping\n\n")
			flusher.Flush()
		case <-r.Context().Done():
			return
		}
	}
}

// ── コスト管理 API ────────────────────────────────────────────────────────────

func handleCosts(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	switch r.Method {
	case http.MethodGet:
		cb, err := costs.Load()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		json.NewEncoder(w).Encode(cb)
	case http.MethodPost:
		var body struct {
			ItemID        string `json:"itemId"`
			PurchasePrice int    `json:"purchasePrice"`
			ShippingCost  int    `json:"shippingCost"`
			Memo          string `json:"memo"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "JSONパース失敗", http.StatusBadRequest)
			return
		}
		if body.ItemID == "" {
			http.Error(w, "itemId required", http.StatusBadRequest)
			return
		}
		if err := costs.Set(body.ItemID, costs.ItemCost{
			PurchasePrice: body.PurchasePrice,
			ShippingCost:  body.ShippingCost,
			Memo:          body.Memo,
		}); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

// ── P&L ページ & データ ────────────────────────────────────────────────────────

func handlePL(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(plHTML)
}

type plItem struct {
	ItemID        string  `json:"itemId"`
	ItemName      string  `json:"itemName"`
	CurrentPrice  int     `json:"currentPrice"`
	PurchasePrice int     `json:"purchasePrice"`
	ShippingCost  int     `json:"shippingCost"`
	Memo          string  `json:"memo"`
	MercariRev    int     `json:"mercariRev"`
	Profit        int     `json:"profit"`
	ProfitRate    float64 `json:"profitRate"`
	HasCost       bool    `json:"hasCost"`
	ImagePath     string  `json:"imagePath,omitempty"`
	ListedDays    int     `json:"listedDays"`
}

func handlePLData(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	csvDir := filepath.Join(projectRoot(), config.DirCSV)
	dirEntries, err := os.ReadDir(csvDir)
	if err != nil {
		json.NewEncoder(w).Encode(map[string]interface{}{"items": []plItem{}, "summary": nil})
		return
	}
	var csvFiles []string
	for _, e := range dirEntries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".csv") {
			csvFiles = append(csvFiles, e.Name())
		}
	}
	if len(csvFiles) == 0 {
		json.NewEncoder(w).Encode(map[string]interface{}{"items": []plItem{}, "summary": nil})
		return
	}
	sort.Sort(sort.Reverse(sort.StringSlice(csvFiles)))

	f, err := os.Open(filepath.Join(csvDir, csvFiles[0]))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer f.Close()
	records, err := csv.NewReader(f).ReadAll()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	cb, _ := costs.Load()
	oldestCSV := csvFiles[len(csvFiles)-1]
	oldestDate, oldestItems := parseCsvDate(oldestCSV, filepath.Join(csvDir, oldestCSV))
	imgDir := filepath.Join(projectRoot(), config.DirImg)

	var items []plItem
	var totalInvested, totalExpectedProfit, noCostCount int

	for i, row := range records {
		if i == 0 || len(row) < 3 {
			continue
		}
		itemID := row[0]
		price, _ := strconv.Atoi(row[2])
		cost := cb[itemID]
		mercariRev := int(float64(price) * 0.9)
		profit := costs.CalcProfit(price, cost.PurchasePrice, cost.ShippingCost)
		var profitRate float64
		if price > 0 {
			profitRate = float64(profit) / float64(price) * 100
		}
		hasCost := cost.PurchasePrice > 0
		listedDays := 0
		if !oldestDate.IsZero() {
			if _, ok := oldestItems[itemID]; ok {
				listedDays = int(time.Since(oldestDate).Hours() / 24)
			}
		}
		item := plItem{
			ItemID: itemID, ItemName: row[1], CurrentPrice: price,
			PurchasePrice: cost.PurchasePrice, ShippingCost: cost.ShippingCost,
			Memo: cost.Memo, MercariRev: mercariRev, Profit: profit,
			ProfitRate: profitRate, HasCost: hasCost, ListedDays: listedDays,
		}
		if matches, _ := filepath.Glob(filepath.Join(imgDir, itemID+".*")); len(matches) > 0 {
			item.ImagePath = "/img/" + filepath.Base(matches[0])
		}
		items = append(items, item)
		if hasCost {
			totalInvested += cost.PurchasePrice + cost.ShippingCost
			totalExpectedProfit += profit
		} else {
			noCostCount++
		}
	}

	var avgRate float64
	if totalInvested > 0 {
		avgRate = float64(totalExpectedProfit) / float64(totalInvested) * 100
	}
	type summary struct {
		TotalItems          int     `json:"totalItems"`
		NoCostCount         int     `json:"noCostCount"`
		TotalInvested       int     `json:"totalInvested"`
		TotalExpectedProfit int     `json:"totalExpectedProfit"`
		AvgProfitRate       float64 `json:"avgProfitRate"`
		TargetMonthly       int     `json:"targetMonthly"`
	}
	json.NewEncoder(w).Encode(map[string]interface{}{
		"items": items,
		"summary": summary{
			TotalItems: len(items), NoCostCount: noCostCount,
			TotalInvested: totalInvested, TotalExpectedProfit: totalExpectedProfit,
			AvgProfitRate: avgRate, TargetMonthly: 300000,
		},
	})
}

func parseCsvDate(filename, fullPath string) (time.Time, map[string]struct{}) {
	parts := strings.Split(strings.TrimSuffix(filename, ".csv"), "_")
	var t time.Time
	if len(parts) >= 1 && len(parts[0]) == 8 {
		t, _ = time.ParseInLocation("20060102", parts[0], time.Local)
	}
	items := map[string]struct{}{}
	f, err := os.Open(fullPath)
	if err != nil {
		return t, items
	}
	defer f.Close()
	records, err := csv.NewReader(f).ReadAll()
	if err != nil {
		return t, items
	}
	for i, row := range records {
		if i == 0 || len(row) < 1 {
			continue
		}
		items[row[0]] = struct{}{}
	}
	return t, items
}

// handleResearchEvents はリサーチ専用の SSE エンドポイントです。
// 値引き・CSV の /events とは独立しており、同時接続可能です。
func handleResearchEvents(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	ch := make(chan string, 200)
	resCliMu.Lock()
	resClients = append(resClients, ch)
	resCliMu.Unlock()

	defer func() {
		resCliMu.Lock()
		for i, c := range resClients {
			if c == ch {
				resClients = append(resClients[:i], resClients[i+1:]...)
				break
			}
		}
		resCliMu.Unlock()
		close(ch)
	}()

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "Streaming not supported", http.StatusInternalServerError)
		return
	}

	resMu.Lock()
	current := resBuf.String()
	resMu.Unlock()
	for _, line := range strings.Split(current, "\n") {
		if line != "" {
			fmt.Fprintf(w, "data: %s\n\n", escapeSSE(line))
		}
	}
	flusher.Flush()

	ticker := time.NewTicker(25 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case msg, ok := <-ch:
			if !ok {
				return
			}
			fmt.Fprintf(w, "data: %s\n\n", escapeSSE(msg))
			flusher.Flush()
		case <-ticker.C:
			fmt.Fprintf(w, ": ping\n\n")
			flusher.Flush()
		case <-r.Context().Done():
			return
		}
	}
}
