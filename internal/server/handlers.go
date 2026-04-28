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
	"strings"
	"time"

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

type historyProductRow struct {
	ItemID    string `json:"itemId"`
	ItemName  string `json:"itemName"`
	ImagePath string `json:"imagePath,omitempty"`
	Prices    []*int `json:"prices"`
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

	// 実行回ごとの商品価格マップ
	runPrices := make([]map[string]int, len(entries))
	var productOrder []string
	productNames := map[string]string{}
	for i, entry := range entries {
		runPrices[i] = map[string]int{}
		for _, p := range entry.Products {
			runPrices[i][p.ItemID] = p.NewPrice
			if _, exists := productNames[p.ItemID]; !exists {
				productOrder = append(productOrder, p.ItemID)
				productNames[p.ItemID] = p.ItemName
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
		prices := make([]*int, len(entries))
		for i := range entries {
			if p, ok := runPrices[i][id]; ok {
				v := p
				prices[i] = &v
			}
		}
		row := historyProductRow{
			ItemID:   id,
			ItemName: productNames[id],
			Prices:   prices,
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
