package main

import (
	"fmt"
	"os"

	"mercari-pricelower/internal/mercari"
)

func main() {
	if err := mercari.EstimateAllMissing(); err != nil {
		fmt.Fprintf(os.Stderr, "送料推定バッチ失敗: %v\n", err)
		os.Exit(1)
	}
}
