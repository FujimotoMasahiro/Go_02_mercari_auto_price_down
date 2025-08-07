package mercari

import (
	"context"
	"fmt"
	"log"
	"strconv"
	"time"

	"github.com/chromedp/chromedp"
)

// MercariClient はメルカリ操作用クライアント
type MercariClient struct {
	ctx context.Context
}

// NewMercariClient コンストラクタ
func NewMercariClient(ctx context.Context) *MercariClient {
	return &MercariClient{ctx: ctx}
}

// Login メルカリのログイン画面へ移動（手動ログイン想定）
func (m *MercariClient) Login() error {
	return chromedp.Run(m.ctx,
		chromedp.Navigate("https://www.mercari.com/jp/login/"),
	)
}

// LowerPrice 指定商品の価格を amount 円だけ下げ、minPrice以下なら停止
func (m *MercariClient) LowerPrice(productID string, amount, minPrice int) error {
	editURL := fmt.Sprintf("https://www.mercari.com/jp/sell/edit/%s/", productID)

	err := chromedp.Run(m.ctx,
		chromedp.Navigate(editURL),
		chromedp.Sleep(3*time.Second),
	)
	if err != nil {
		return err
	}

	var currentPriceStr string
	err = chromedp.Run(m.ctx,
		chromedp.Value(`input[name="price"]`, &currentPriceStr, chromedp.ByQuery),
	)
	if err != nil {
		return err
	}

	currentPrice, err := strconv.Atoi(currentPriceStr)
	if err != nil {
		return err
	}

	newPrice := currentPrice - amount
	if newPrice < minPrice {
		log.Printf("商品ID:%s は最低価格(%d円)を下回るため値下げをスキップします。\n", productID, minPrice)
		return nil
	}

	err = chromedp.Run(m.ctx,
		chromedp.SetValue(`input[name="price"]`, "", chromedp.ByQuery),
		chromedp.SendKeys(`input[name="price"]`, strconv.Itoa(newPrice), chromedp.ByQuery),
		chromedp.Click(`button:contains("変更を保存")`, chromedp.ByQueryAll),
		chromedp.Sleep(2*time.Second),
	)
	if err != nil {
		return err
	}

	log.Printf("商品ID:%s の価格を %d円 → %d円 に値下げしました。\n", productID, currentPrice, newPrice)
	return nil
}
