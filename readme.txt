//　下記でバイナリファイルを作成
go build -o bin/メルカリ自動値引きツール_mac_ver100

GOOS=windows GOARCH=amd64 go build -o bin/メルカリ自動値引きツール_windows_ver100.exe
