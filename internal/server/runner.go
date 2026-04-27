package server

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
	"sync"
)

//go:embed build_targets.json
var buildTargetsJSON []byte

// BuildTarget はビルド対象バイナリの定義です。
// internal/server/build_targets.json に追加することで保守できます。
type BuildTarget struct {
	Name   string `json:"name"`
	GOOS   string `json:"goos"` // "darwin" / "windows" / "" (全OS)
	Output string `json:"output"`
	Pkg    string `json:"pkg"`
}

func startBinary(w http.ResponseWriter, finder func() string, extraArgs ...string) {
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

	go runBinary(finder, extraArgs...)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "started"})
}

func runBinary(finder func() string, extraArgs ...string) {
	logFile := openLogFile()
	defer func() {
		if logFile != nil {
			logFile.Sync()
			logFile.Close()
		}
		mu.Lock()
		isRunning = false
		mu.Unlock()
		broadcast("__DONE__")
	}()

	var logMu sync.Mutex
	writeLine := func(line string) {
		broadcast(line)
		if logFile != nil {
			logMu.Lock()
			fmt.Fprintln(logFile, line)
			logMu.Unlock()
		}
	}

	binaryPath := finder()
	if binaryPath == "" {
		writeLine("ERROR: バイナリが見つかりません (bin/ ディレクトリを確認してください)")
		return
	}

	writeLine("[起動] " + binaryPath)

	cmd := exec.Command(binaryPath, extraArgs...)
	cmd.Dir = projectRoot()

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		writeLine("ERROR: " + err.Error())
		return
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		writeLine("ERROR: " + err.Error())
		return
	}

	if err := cmd.Start(); err != nil {
		writeLine("ERROR: 起動失敗: " + err.Error())
		return
	}

	var wg sync.WaitGroup
	pipe := func(r io.Reader) {
		defer wg.Done()
		scanner := bufio.NewScanner(r)
		for scanner.Scan() {
			writeLine(scanner.Text())
		}
	}
	wg.Add(2)
	go pipe(stdout)
	go pipe(stderr)
	wg.Wait()

	if err := cmd.Wait(); err != nil {
		writeLine("[終了] エラー: " + err.Error())
	} else {
		writeLine("[終了] 正常終了")
	}
}

func buildAll() {
	var targets []BuildTarget
	if err := json.Unmarshal(buildTargetsJSON, &targets); err != nil {
		log.Printf("[ビルド] 設定読み込み失敗: %v", err)
		return
	}

	root := projectRoot()
	for _, t := range targets {
		if t.GOOS != "" && t.GOOS != runtime.GOOS {
			continue
		}
		pkg := t.Pkg
		if pkg == "" {
			pkg = "."
		}
		output := filepath.Join(root, t.Output)
		log.Printf("[ビルド] %s をビルド中...", t.Name)
		cmd := exec.Command("go", "build", "-o", output, pkg)
		cmd.Dir = root
		if out, err := cmd.CombinedOutput(); err != nil {
			log.Printf("[ビルド失敗] %s: %v\n%s", t.Name, err, string(out))
		} else {
			log.Printf("[ビルド完了] %s → %s", t.Name, t.Output)
		}
	}
}

func findBinary() string {
	return findBinaryByName("メルカリ自動値引きツール_mac_ver100", "メルカリ自動値引きツール_windows_ver100.exe")
}

func findCSVBinary() string {
	return findBinaryByName("メルカリCSV作成ツール_mac_ver100", "メルカリCSV作成ツール_windows_ver100.exe")
}

func findBinaryByName(macName, winName string) string {
	root := projectRoot()

	var name string
	if runtime.GOOS == "windows" {
		name = winName
	} else {
		name = macName
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
