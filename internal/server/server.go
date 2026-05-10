package server

import (
	_ "embed"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"log"

	"mercari-pricelower/internal/config"
	"mercari-pricelower/internal/db"
)

//go:embed templates/index.html
var indexHTML []byte

//go:embed templates/products.html
var productsHTML []byte

//go:embed templates/history.html
var historyHTML []byte

//go:embed templates/research.html
var researchHTML []byte

//go:embed templates/settings.html
var settingsHTML []byte

//go:embed templates/pl.html
var plHTML []byte

var (
	// 値引き・CSV操作用（research とは独立して動作する）
	mu        sync.Mutex
	isRunning bool
	outputBuf strings.Builder

	clientsMu sync.RWMutex
	clients   []chan string

	// リサーチ専用（値引き・CSVとは独立して同時実行可能）
	resMu      sync.Mutex
	resRunning bool
	resBuf     strings.Builder

	resCliMu  sync.RWMutex
	resClients []chan string
)

// LogDirInfo はログディレクトリとそのファイル一覧を表します。
type LogDirInfo struct {
	Name  string   `json:"name"`
	Files []string `json:"files"`
}

// ProductItem は商品一覧の1行を表します。
type ProductItem struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Price     string `json:"price"`
	ImagePath string `json:"imagePath,omitempty"`
}

// Run はWebサーバーを起動します。
func Run() {
	buildAll()

	// DB接続（config.json に database 設定がある場合のみ）
	if cfg := config.Cfg.Database; cfg != nil && cfg.Host != "" {
		if err := db.Open(cfg.DSN()); err != nil {
			log.Printf("[DB] 接続失敗（CSV フォールバックで動作します）: %v", err)
		} else {
			defer db.Close()
		}
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", handleIndex)
	mux.HandleFunc("/products", handleProducts)
	mux.HandleFunc("/history", handleHistory)
	mux.HandleFunc("/history-data", handleHistoryData)
	mux.HandleFunc("/csv-data", handleCSVData)
	mux.HandleFunc("/research", handleResearch)
	mux.HandleFunc("/research-data", handleResearchData)
	mux.HandleFunc("/research-live", handleResearchLive)
	mux.HandleFunc("/run-research", handleRunResearch)

	imgDir := filepath.Join(projectRoot(), config.DirImg)
	os.MkdirAll(imgDir, 0755)
	mux.Handle("/img/", http.StripPrefix("/img/", http.FileServer(http.Dir(imgDir))))

	researchImgDir := filepath.Join(projectRoot(), config.DirResearchImg)
	os.MkdirAll(researchImgDir, 0755)
	mux.Handle("/research_img/", http.StripPrefix("/research_img/", http.FileServer(http.Dir(researchImgDir))))

	mux.HandleFunc("/pl", handlePL)
	mux.HandleFunc("/pl-data", handlePLData)
	mux.HandleFunc("/api/costs", handleCosts)
	mux.HandleFunc("/settings", handleSettings)
	mux.HandleFunc("/api/config", handleConfig)

	mux.HandleFunc("/run", handleRun)
	mux.HandleFunc("/create-csv", handleCreateCSV)
	mux.HandleFunc("/stop", handleStop)
	mux.HandleFunc("/status", handleStatus)
	mux.HandleFunc("/logs", handleLogs)
	mux.HandleFunc("/logfile", handleLogFile)
	mux.HandleFunc("/events", handleEvents)
	mux.HandleFunc("/events/research", handleResearchEvents)

	// ポートを自動選択 (8080 〜 8089)
	const basePort = 8080
	var listener net.Listener
	var chosenPort int
	for p := basePort; p <= basePort+9; p++ {
		ln, err := net.Listen("tcp", fmt.Sprintf(":%d", p))
		if err == nil {
			listener = ln
			chosenPort = p
			break
		}
		// 8080 が使用中の場合、自分自身が既に起動しているか確認
		if p == basePort {
			statusURL := fmt.Sprintf("http://localhost:%d/status", p)
			if resp, err2 := http.Get(statusURL); err2 == nil {
				resp.Body.Close()
				url := fmt.Sprintf("http://localhost:%d", p)
				log.Printf("Web UI は既に起動中です: %s をブラウザで開きます", url)
				openBrowser(url)
				return
			}
		}
		log.Printf("ポート %d は使用中のため %d を試みます...", p, p+1)
	}
	if listener == nil {
		log.Fatal("利用可能なポートが見つかりません (8080-8089)")
	}

	url := fmt.Sprintf("http://localhost:%d", chosenPort)
	log.Printf("Web UI 起動: %s", url)
	go openBrowser(url)
	log.Fatal(http.Serve(listener, mux))
}

func broadcast(msg string) {
	mu.Lock()
	outputBuf.WriteString(msg + "\n")
	mu.Unlock()

	clientsMu.RLock()
	defer clientsMu.RUnlock()
	for _, ch := range clients {
		select {
		case ch <- msg:
		default:
		}
	}
}

func broadcastResearch(msg string) {
	resMu.Lock()
	resBuf.WriteString(msg + "\n")
	resMu.Unlock()

	resCliMu.RLock()
	defer resCliMu.RUnlock()
	for _, ch := range resClients {
		select {
		case ch <- msg:
		default:
		}
	}
}

func escapeSSE(s string) string {
	return strings.ReplaceAll(s, "\n", " ")
}

func projectRoot() string {
	exe, err := os.Executable()
	if err != nil {
		return "."
	}
	dir := filepath.Dir(exe)
	if filepath.Base(dir) == "bin" {
		return filepath.Dir(dir)
	}
	// go run ./cmd/server/  →  exe is in /tmp/…, use cwd
	cwd, _ := os.Getwd()
	return cwd
}

func findLogDirs() []LogDirInfo {
	root := projectRoot()
	logDir := filepath.Join(root, config.DirLog)
	entries, err := os.ReadDir(logDir)
	if err != nil {
		return nil
	}
	var logFiles []string
	for _, f := range entries {
		if !f.IsDir() && strings.HasSuffix(f.Name(), ".log") {
			logFiles = append(logFiles, f.Name())
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(logFiles)))
	return []LogDirInfo{{Name: config.DirLog, Files: logFiles}}
}

func openLogFile() *os.File {
	logDir := filepath.Join(projectRoot(), config.DirLog)
	if err := os.MkdirAll(logDir, 0755); err != nil {
		log.Printf("ログディレクトリ作成失敗: %v", err)
		return nil
	}
	logPath := filepath.Join(logDir, time.Now().Format("20060102")+".log")
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		log.Printf("ログファイル作成失敗: %v", err)
		return nil
	}
	return f
}
