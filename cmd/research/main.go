package main

import (
	"flag"
	"fmt"
	"os"
	"strings"

	"mercari-pricelower/internal/mercari"
)

func main() {
	listFlag := flag.Bool("list", false, "カテゴリー一覧を表示")
	categoryID := flag.String("category-id", "", "カテゴリーID (例: 5)")
	categoryName := flag.String("category", "すべて", "カテゴリー名 (例: レディース)")
	pages := flag.Int("pages", 5, "取得ページ数 (1〜10)")
	flag.Parse()

	if *listFlag {
		printCategories()
		return
	}

	if *pages < 1 {
		*pages = 1
	}
	if *pages > 10 {
		*pages = 10
	}

	catID := *categoryID
	catName := *categoryName

	// -category-id が未指定の場合、-category 名からIDを解決
	if catID == "" && catName != "すべて" {
		for _, c := range mercari.CategoryList {
			if c.Name == catName {
				catID = c.ID
				break
			}
		}
		if catID == "" {
			fmt.Fprintf(os.Stderr, "カテゴリー名が見つかりません: %q\n", catName)
			fmt.Fprintln(os.Stderr, "利用可能なカテゴリーを確認するには -list フラグを使用してください")
			os.Exit(1)
		}
	}

	// -category-id が指定された場合はカテゴリー名をIDから解決
	if *categoryID != "" {
		catID = *categoryID
		for _, c := range mercari.CategoryList {
			if c.ID == catID {
				catName = c.Name
				break
			}
		}
		if catName == "すべて" && catID != "" {
			catName = "カテゴリーID:" + catID
		}
	}

	fmt.Printf("リサーチ開始: カテゴリー=%s, ページ数=%d\n", catName, *pages)
	mercari.RunResearch(catID, catName, *pages)
}

func printCategories() {
	fmt.Println("利用可能なカテゴリー一覧:")
	fmt.Println(strings.Repeat("-", 40))
	for _, c := range mercari.CategoryList {
		id := c.ID
		if id == "" {
			id = "(なし)"
		}
		fmt.Printf("  ID: %-8s  名前: %s\n", id, c.Name)
	}
	fmt.Println(strings.Repeat("-", 40))
	fmt.Println("使用例:")
	fmt.Println("  go run ./cmd/research -category=レディース -pages=5")
	fmt.Println("  go run ./cmd/research -category-id=9 -pages=5")
}
