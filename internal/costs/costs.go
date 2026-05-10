package costs

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"mercari-pricelower/internal/config"
)

type ItemCost struct {
	PurchasePrice int       `json:"purchasePrice"` // 仕入れ値（円）
	ShippingCost  int       `json:"shippingCost"`  // 発送送料（円）
	Memo          string    `json:"memo"`
	UpdatedAt     time.Time `json:"updatedAt"`
}

// map[itemId]ItemCost
type CostBook map[string]ItemCost

func filePath() string {
	return filepath.Join(config.DirData, "costs.json")
}

func Load() (CostBook, error) {
	data, err := os.ReadFile(filePath())
	if os.IsNotExist(err) {
		return CostBook{}, nil
	}
	if err != nil {
		return nil, err
	}
	var cb CostBook
	if err := json.Unmarshal(data, &cb); err != nil {
		return nil, err
	}
	return cb, nil
}

func Save(cb CostBook) error {
	if err := os.MkdirAll(config.DirData, 0755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(cb, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filePath(), data, 0644)
}

func Set(itemID string, cost ItemCost) error {
	cb, err := Load()
	if err != nil {
		return err
	}
	cost.UpdatedAt = time.Now()
	cb[itemID] = cost
	return Save(cb)
}

// CalcProfit は出品価格から期待純利益を計算します。
// 利益 = floor(sellPrice × 0.9) - purchasePrice - shippingCost
func CalcProfit(sellPrice, purchasePrice, shippingCost int) int {
	mercariRevenue := int(float64(sellPrice) * 0.9)
	return mercariRevenue - purchasePrice - shippingCost
}
