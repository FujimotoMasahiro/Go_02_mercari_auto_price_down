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
	mux.HandleFunc("/logfile", handleLogFile)
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

// LogDirInfo holds a directory name and its log files (newest first).
type LogDirInfo struct {
	Name  string   `json:"name"`
	Files []string `json:"files"`
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

	// Security: path must be under one of the known log dirs.
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

// findLogDirs scans the project root for directories whose names end with
// "Log" or "log" (e.g. MerucariLog, Log, RakutenLog …) and returns one
// LogDirInfo per directory with its .log files sorted newest-first.
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
		lower := strings.ToLower(name)
		if !strings.HasSuffix(lower, "log") {
			continue
		}

		dirPath := filepath.Join(root, name)
		files, err := os.ReadDir(dirPath)
		if err != nil {
			continue
		}

		var logFiles []string
		for _, f := range files {
			if !f.IsDir() && strings.HasSuffix(f.Name(), ".log") {
				logFiles = append(logFiles, f.Name())
			}
		}
		// Sort newest-first (filenames are date-stamped, so reverse alpha = newest first)
		sort.Sort(sort.Reverse(sort.StringSlice(logFiles)))

		result = append(result, LogDirInfo{Name: name, Files: logFiles})
	}

	// Sort dirs alphabetically for stable ordering
	sort.Slice(result, func(i, j int) bool { return result[i].Name < result[j].Name })
	return result
}
