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

## ビルド

```bash
# macOS バイナリ
go build -o bin/メルカリ自動値引きツール_mac_ver100

# Windows バイナリ
GOOS=windows GOARCH=amd64 go build -o bin/メルカリ自動値引きツール_windows_ver100.exe
```
