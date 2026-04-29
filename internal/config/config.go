package config

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// Version はアプリケーションのバージョンです（コード管理）。
const Version = "1.0.0"

// AppConfig は config.json から読み込むアプリケーション設定です。
type AppConfig struct {
	// Headless: true=ヘッドレスモード（ブラウザ非表示） false=通常モード（ブラウザ表示）
	Headless bool `json:"headless"`

	// MinPrice: 値下げ後の最低価格（円）。これを下回る場合はスキップ。
	MinPrice int `json:"min_price"`

	// PriceDecreaseAmount: 値下げ幅（円）。将来の拡張用。
	PriceDecreaseAmount int `json:"price_decrease_amount"`
}

// Cfg はアプリ全体で参照する設定値です。
// 起動時に config.json を読み込み、存在しない場合はデフォルト値を使用します。
var Cfg = defaultConfig()

func init() {
	// エラーは無視してデフォルト値のまま起動を継続する
	_ = loadConfig()
}

// defaultConfig はデフォルト設定を返します。
func defaultConfig() AppConfig {
	return AppConfig{
		Headless:            false,
		MinPrice:            3000,
		PriceDecreaseAmount: 100,
	}
}

// loadConfig は config.json を探して読み込みます。
func loadConfig() error {
	path := findConfigFile()
	f, err := os.Open(path)
	if err != nil {
		// ファイルが存在しない場合はデフォルト値のまま
		return err
	}
	defer f.Close()

	// デフォルト値をベースにデコードすることで、
	// config.json に書いていないキーはデフォルト値が維持される
	loaded := defaultConfig()
	if err := json.NewDecoder(f).Decode(&loaded); err != nil {
		return err
	}
	Cfg = loaded
	return nil
}

// findConfigFile は config.json のパスを次の順で探します。
//  1. カレントディレクトリ（go run / 開発時はプロジェクトルート）
//  2. 実行バイナリと同じディレクトリ（bin/ に配置した場合）
//  3. 実行バイナリの1つ上のディレクトリ（bin/ → プロジェクトルート）
func findConfigFile() string {
	const name = "config.json"

	// 1. カレントディレクトリ
	if _, err := os.Stat(name); err == nil {
		return name
	}

	// 2 & 3. 実行バイナリ基準
	if exe, err := os.Executable(); err == nil {
		dir := filepath.Dir(exe)

		// 実行バイナリと同じ場所
		if p := filepath.Join(dir, name); fileExists(p) {
			return p
		}
		// bin/ の場合は1つ上（プロジェクトルート）
		if p := filepath.Join(dir, "..", name); fileExists(p) {
			return p
		}
	}

	// 見つからなければカレントディレクトリを返す（読み込みはエラーになるがデフォルト値で起動）
	return name
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// Save は設定を config.json に書き込み、Cfg をメモリ上でも更新します。
// 次回スクレイピング実行時から新しい設定が反映されます。
func Save(cfg AppConfig) error {
	path := findConfigFile()
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, data, 0644); err != nil {
		return err
	}
	Cfg = cfg
	return nil
}
