// test_mcp/main.go
package main

import (
	"fmt"
	"os"
)

func main() {
	url := "http://localhost:8080/mcp"
	if len(os.Args) > 1 {
		url = os.Args[1]
	}

	fmt.Fprintf(os.Stderr, "Connecting to %s\n", url)
	c, err := ConnectStreamableHTTP(url)
	if err != nil {
		fmt.Fprintf(os.Stderr, "❌ Failed: %v\n", err)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "✅ Connected, %d tools:\n", len(c.Tools()))
	for _, t := range c.Tools() {
		fmt.Fprintf(os.Stderr, "  • %s\n", t.Name)
	}
}
