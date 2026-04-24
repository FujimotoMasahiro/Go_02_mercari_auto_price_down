package main

import (
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"time"
)

// AppLogger はファイルと標準出力に同時に書き込むロガーです。
type AppLogger struct {
	logger *log.Logger
	file   *os.File
}

var appLogger *AppLogger

// NewAppLogger は Log ディレクトリに日付ベースのログファイルを作成します。
// 同じ日の実行では追記されます。
func NewAppLogger() (*AppLogger, error) {
	logDir := "Log"
	if err := os.MkdirAll(logDir, 0755); err != nil {
		return nil, fmt.Errorf("ログディレクトリの作成失敗: %w", err)
	}

	logFileName := time.Now().Format("20060102") + ".log"
	logFilePath := filepath.Join(logDir, logFileName)

	logFile, err := os.OpenFile(logFilePath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		return nil, fmt.Errorf("ログファイルの作成失敗: %w", err)
	}

	writer := io.MultiWriter(os.Stdout, logFile)
	logger := log.New(writer, "", 0)

	return &AppLogger{logger: logger, file: logFile}, nil
}

// Close はログファイルをクローズします。
func (l *AppLogger) Close() {
	if l.file != nil {
		l.file.Close()
	}
}

func (l *AppLogger) write(level, screen, action, status string) {
	ts := time.Now().Format("2006/01/02 15:04:05")
	l.logger.Printf("%s [%s] [%-20s] %s → %s", ts, level, screen, action, status)
}

// Info は通常ログを記録します。
func (l *AppLogger) Info(screen, action, status string) {
	l.write("INFO ", screen, action, status)
}

// Warn は警告ログを記録します。
func (l *AppLogger) Warn(screen, action, status string) {
	l.write("WARN ", screen, action, status)
}

// Error はエラーログを記録します。
func (l *AppLogger) Error(screen, action, status string) {
	l.write("ERROR", screen, action, status)
}

// Separator はセッション開始の区切り線を挿入します。
func (l *AppLogger) Separator() {
	ts := time.Now().Format("2006/01/02 15:04:05")
	l.logger.Printf("%s ================================================", ts)
}
