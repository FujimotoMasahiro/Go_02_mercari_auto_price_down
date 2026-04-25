package main

import (
	"log"
	"os"
	"time"
)

// AppLogger は標準出力に書き込むロガーです。
type AppLogger struct {
	logger *log.Logger
}

var appLogger *AppLogger

// NewAppLogger はロガーを初期化します。
func NewAppLogger() (*AppLogger, error) {
	logger := log.New(os.Stdout, "", 0)
	return &AppLogger{logger: logger}, nil
}

// Close は互換性のために残しています（ファイルなし）。
func (l *AppLogger) Close() {}

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
