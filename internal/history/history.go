package history

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

const maxEntries = 50

type ProductDiscount struct {
	ItemID   string `json:"itemId"`
	ItemName string `json:"itemName"`
	OldPrice int    `json:"oldPrice"`
	NewPrice int    `json:"newPrice"`
}

type SkippedProduct struct {
	ItemID   string `json:"itemId"`
	ItemName string `json:"itemName"`
	Price    int    `json:"price"`
	Reason   string `json:"reason"`
}

type Entry struct {
	Timestamp time.Time         `json:"timestamp"`
	Products  []ProductDiscount `json:"products"`
	Skipped   []SkippedProduct  `json:"skipped,omitempty"`
}

func filePath() string {
	return filepath.Join("history", "price_history.json")
}

func Load() ([]Entry, error) {
	data, err := os.ReadFile(filePath())
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var entries []Entry
	if err := json.Unmarshal(data, &entries); err != nil {
		return nil, err
	}
	return entries, nil
}

func Append(entry Entry) error {
	entries, _ := Load()
	entries = append([]Entry{entry}, entries...)
	if len(entries) > maxEntries {
		entries = entries[:maxEntries]
	}
	data, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll("history", 0755); err != nil {
		return err
	}
	return os.WriteFile(filePath(), data, 0644)
}
