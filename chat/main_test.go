// mcp_streamable_test.go
package main

import (
	"fmt"
	"testing"
)

func TestStreamableHTTP(t *testing.T) {
	url := "http://localhost:8080/mcp"

	fmt.Printf("Connecting to %s\n", url)
	c, err := ConnectStreamableHTTP(url)
	if err != nil {
		t.Fatalf("❌ Failed: %v", err)
	}
	fmt.Printf("✅ Connected, %d tools:\n", len(c.Tools()))
	for _, tool := range c.Tools() {
		fmt.Printf("  • %s\n", tool.Name)
	}
}
