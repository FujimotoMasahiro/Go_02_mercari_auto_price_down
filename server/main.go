package main

import (
	"bufio"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
)

//go:embed templates/index.html
var indexHTML []byte

var (
	mu        sync.Mutex
	isRunning bool
	outputBuf strings.Builder

	clientsMu sync.RWMutex
	clients   []chan string
)

func main() {
	mux := http.NewServeMux()
	mux.HandleFunc("/", handleIndex)
	mux.HandleFunc("/run", handleRun)
	mux.HandleFunc("/stop", handleStop)
	mux.HandleFunc("/status", handleStatus)
	mux.HandleFunc("/logs", handleLogs)
	mux.HandleFunc("/events", handleEvents)

	addr := ":8080"
	log.Printf("Web UI 起動: http://localhost%s", addr)
	log.Fatal(http.ListenAndServe(addr, mux))
}

func handleIndex(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(indexHTML)
}

func handleRun(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	mu.Lock()
	if isRunning {
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		json.NewEncoder(w).Encode(map[string]string{"error": "実行中です"})
		return
	}
	isRunning = true
	outputBuf.Reset()
	mu.Unlock()

	go runBinary()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "started"})
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
	logDir := findLogDir()
	if logDir == "" {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{
			"content":  "ログディレクトリが見つかりません",
			"filename": "",
		})
		return
	}

	entries, err := os.ReadDir(logDir)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{
			"content":  "ログを読み込めませんでした: " + err.Error(),
			"filename": "",
		})
		return
	}

	var logFiles []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".log") {
			logFiles = append(logFiles, filepath.Join(logDir, e.Name()))
		}
	}

	if len(logFiles) == 0 {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{
			"content":  "ログファイルがありません",
			"filename": "",
		})
		return
	}

	sort.Strings(logFiles)
	latestLog := logFiles[len(logFiles)-1]

	content, err := os.ReadFile(latestLog)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{
			"content":  "ログを読み込めませんでした",
			"filename": "",
		})
		return
	}

	lines := strings.Split(string(content), "\n")
	if len(lines) > 200 {
		lines = lines[len(lines)-200:]
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"content":  strings.Join(lines, "\n"),
		"filename": filepath.Base(latestLog),
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

	// Replay buffered output to new client
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

func escapeSSE(s string) string {
	return strings.ReplaceAll(s, "\n", " ")
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

func runBinary() {
	defer func() {
		mu.Lock()
		isRunning = false
		mu.Unlock()
		broadcast("__DONE__")
	}()

	binaryPath := findBinary()
	if binaryPath == "" {
		var name string
		if runtime.GOOS == "windows" {
			name = "メルカリ自動値引きツール_windows_ver100.exe"
		} else {
			name = "メルカリ自動値引きツール_mac_ver100"
		}
		broadcast("ERROR: バイナリが見つかりません: bin/" + name)
		return
	}

	broadcast("[起動] " + binaryPath)

	cmd := exec.Command(binaryPath)
	cmd.Dir = projectRoot()

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		broadcast("ERROR: " + err.Error())
		return
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		broadcast("ERROR: " + err.Error())
		return
	}

	if err := cmd.Start(); err != nil {
		broadcast("ERROR: 起動失敗: " + err.Error())
		return
	}

	var wg sync.WaitGroup
	pipe := func(r io.Reader) {
		defer wg.Done()
		scanner := bufio.NewScanner(r)
		for scanner.Scan() {
			broadcast(scanner.Text())
		}
	}
	wg.Add(2)
	go pipe(stdout)
	go pipe(stderr)
	wg.Wait()

	if err := cmd.Wait(); err != nil {
		broadcast("[終了] エラー: " + err.Error())
	} else {
		broadcast("[終了] 正常終了")
	}
}

func projectRoot() string {
	// Server binary lives in bin/, so executable's parent = project root.
	exe, err := os.Executable()
	if err != nil {
		return "."
	}
	// When built to bin/web-server:
	dir := filepath.Dir(exe)
	if filepath.Base(dir) == "bin" {
		return filepath.Dir(dir)
	}
	// go run ./server/  →  exe is in /tmp/…, use cwd
	cwd, _ := os.Getwd()
	return cwd
}

func findBinary() string {
	root := projectRoot()

	var name string
	if runtime.GOOS == "windows" {
		name = "メルカリ自動値引きツール_windows_ver100.exe"
	} else {
		name = "メルカリ自動値引きツール_mac_ver100"
	}

	candidates := []string{
		filepath.Join(root, "bin", name),
		filepath.Join("bin", name),
		filepath.Join("..", "bin", name),
	}
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			abs, _ := filepath.Abs(c)
			return abs
		}
	}
	return ""
}

func findLogDir() string {
	root := projectRoot()
	for _, d := range []string{
		filepath.Join(root, "Log"),
		"Log",
		filepath.Join("..", "Log"),
	} {
		if info, err := os.Stat(d); err == nil && info.IsDir() {
			abs, _ := filepath.Abs(d)
			return abs
		}
	}
	return ""
}
