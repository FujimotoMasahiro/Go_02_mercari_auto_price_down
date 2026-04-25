# Go_02_mercari_auto_price_down

メルカリの商品価格を自動的に値下げするツール。

## 自動実行スケジュール（launchd）

macOS の launchd により、毎日 **10:00 AM** に自動実行されます。

ログは `Log/launchd_stdout.log` / `Log/launchd_stderr.log` に出力されます。

### 管理コマンド

```bash
# 停止・解除
launchctl unload ~/Library/LaunchAgents/com.fujimotomasahiro.mercari-auto-price-down.plist

# 再登録（設定変更後）
launchctl load ~/Library/LaunchAgents/com.fujimotomasahiro.mercari-auto-price-down.plist

# 手動実行テスト
launchctl start com.fujimotomasahiro.mercari-auto-price-down

# 状態確認
launchctl list | grep mercari-auto-price-down
```

### 手動実行

```bash
cd /Users/fujimotomasahiro/Documents/Golang/Go_02_mercari_auto_price_down
go run .
```

## Web UI（管理画面）

ブラウザから値引き実行・ログ確認ができる管理画面を起動できます。

```bash
# ビルドして起動（プロジェクトルートから）
go build -o bin/web-server ./server/
./bin/web-server
```

または直接実行：

```bash
go run ./server/
```

起動後、ブラウザで http://localhost:8080 を開いてください。

- **値引きを実行する** ボタンで値引きバイナリを起動
- 実行ログをリアルタイム表示
- ログファイルの内容を画面下部で確認

## ビルド

```bash
# macOS バイナリ
go build -o bin/メルカリ自動値引きツール_mac_ver100

# Windows バイナリ
GOOS=windows GOARCH=amd64 go build -o bin/メルカリ自動値引きツール_windows_ver100.exe

# Web UI サーバー
go build -o bin/web-server ./server/
```
