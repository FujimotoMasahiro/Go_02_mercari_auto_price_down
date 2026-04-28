package server

import (
	_ "embed"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"log"
)

//go:embed templates/index.html
var indexHTML []byte

//go:embed templates/products.html
var productsHTML []byte

//go:embed templates/history.html
var historyHTML []byte

var (
	mu        sync.Mutex
	isRunning bool
	outputBuf strings.Builder

	clientsMu sync.RWMutex
	clients   []chan string
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

	mux := http.NewServeMux()
	mux.HandleFunc("/", handleIndex)
	mux.HandleFunc("/products", handleProducts)
	mux.HandleFunc("/history", handleHistory)
	mux.HandleFunc("/history-data", handleHistoryData)
	mux.HandleFunc("/csv-data", handleCSVData)

	imgDir := filepath.Join(projectRoot(), "img")
	os.MkdirAll(imgDir, 0755)
	mux.Handle("/img/", http.StripPrefix("/img/", http.FileServer(http.Dir(imgDir))))

	mux.HandleFunc("/run", handleRun)
	mux.HandleFunc("/create-csv", handleCreateCSV)
	mux.HandleFunc("/stop", handleStop)
	mux.HandleFunc("/status", handleStatus)
	mux.HandleFunc("/logs", handleLogs)
	mux.HandleFunc("/logfile", handleLogFile)
	mux.HandleFunc("/events", handleEvents)

	addr := ":8080"
	url := "http://localhost" + addr
	log.Printf("Web UI 起動: %s", url)
	go openBrowser(url)
	log.Fatal(http.ListenAndServe(addr, mux))
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
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil
	}

	var result []LogDirInfo
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(strings.ToLower(name), "log") {
			continue
		}

		files, err := os.ReadDir(filepath.Join(root, name))
		if err != nil {
			continue
		}

		var logFiles []string
		for _, f := range files {
			if !f.IsDir() && strings.HasSuffix(f.Name(), ".log") {
				logFiles = append(logFiles, f.Name())
			}
		}
		sort.Sort(sort.Reverse(sort.StringSlice(logFiles)))
		result = append(result, LogDirInfo{Name: name, Files: logFiles})
	}

	sort.Slice(result, func(i, j int) bool { return result[i].Name < result[j].Name })
	return result
}

func openLogFile() *os.File {
	logDir := filepath.Join(projectRoot(), "Log")
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
