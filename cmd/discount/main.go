package main

import (
	"flag"
	"mercari-pricelower/internal/mercari"
	"strings"
)

func main() {
	var excludeFlag string
	flag.StringVar(&excludeFlag, "exclude", "", "comma-separated product IDs to exclude from discounting")
	flag.Parse()

	var excludedIDs []string
	if excludeFlag != "" {
		for _, id := range strings.Split(excludeFlag, ",") {
			id = strings.TrimSpace(id)
			if id != "" {
				excludedIDs = append(excludedIDs, id)
			}
		}
	}

	mercari.RunDiscount(excludedIDs)
}
