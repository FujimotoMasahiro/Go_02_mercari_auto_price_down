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
	w.Write(historyHTML)
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

	imgDir := filepath.Join(projectRoot(), "img")
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

	csvDir := filepath.Join(projectRoot(), "CSV")
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

	imgDir := filepath.Join(projectRoot(), "img")
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

	researchDir := filepath.Join(projectRoot(), "research")
	entries, err := os.ReadDir(researchDir)
	if err != nil {
		json.NewEncoder(w).Encode(map[string]interface{}{"items": []map[string]interface{}{}, "file": ""})
		return
	}

	var csvFiles []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".csv") {
			csvFiles = append(csvFiles, e.Name())
		}
	}
	if len(csvFiles) == 0 {
		json.NewEncoder(w).Encode(map[string]interface{}{"items": []map[string]interface{}{}, "file": ""})
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

	type researchItem struct {
		ID        string `json:"id"`
		Name      string `json:"name"`
		Price     int    `json:"price"`
		SoldIn    string `json:"soldIn,omitempty"`
		ImagePath string `json:"imagePath,omitempty"`
	}
	researchImgDir := filepath.Join(projectRoot(), "research_img")
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
		item := researchItem{ID: row[0], Name: row[1], Price: price, SoldIn: soldIn}
		if matches, _ := filepath.Glob(filepath.Join(researchImgDir, row[0]+".*")); len(matches) > 0 {
			item.ImagePath = "/research_img/" + filepath.Base(matches[0])
		}
		items = append(items, item)
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"items": items,
		"file":  latestFile,
	})
}

func handleRunResearch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var body struct {
		CategoryID   string `json:"categoryId"`
		CategoryName string `json:"categoryName"`
		Pages        int    `json:"pages"`
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

	extraArgs := []string{
		"-category-id=" + body.CategoryID,
		"-category=" + body.CategoryName,
		fmt.Sprintf("-pages=%d", body.Pages),
	}
	startBinary(w, findResearchBinary, extraArgs...)
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

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"running": running,
		"output":  output,
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
