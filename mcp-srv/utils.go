package main

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	html2md "github.com/JohannesKaufmann/html-to-markdown"
	mcp "github.com/mark3labs/mcp-go/mcp"
)

var shellCharacters = []string{"|", ">", "<", "&", ";", "\n", "\r"}
var envVarRegex = regexp.MustCompile(`\$(?:[a-zA-Z_][a-zA-Z0-9_]*|\{[a-zA-Z_][a-zA-Z0-9_]*\})`)

// RequiresShell inspects a command string to see if it needs a shell interpreter.
func RequiresShell(cmdStr string) bool {
	cmdStr = strings.TrimSpace(cmdStr)
	if cmdStr == "" {
		return false
	}

	for _, char := range shellCharacters {
		if strings.Contains(cmdStr, char) {
			return true
		}
	}

	if envVarRegex.MatchString(cmdStr) {
		return true
	}

	if strings.Contains(cmdStr, "*") || strings.Contains(cmdStr, "?") {
		return true
	}

	return false
}

// argString extracts a named string argument from a CallToolRequest, returning
// "" if the argument is absent or not a string.
func argString(req mcp.CallToolRequest, key string) string {
	args, ok := req.Params.Arguments.(map[string]any)
	if !ok {
		return ""
	}
	v, _ := args[key].(string)
	return v
}

// argInt extracts a named numeric argument, returning defaultVal if absent or not a number.
// JSON numbers unmarshal as float64 in an any map.
func argInt(req mcp.CallToolRequest, key string, defaultVal int) int {
	args, ok := req.Params.Arguments.(map[string]any)
	if !ok {
		return defaultVal
	}
	switch v := args[key].(type) {
	case float64:
		return int(v)
	case int:
		return v
	}
	return defaultVal
}

// makeReq builds a CallToolRequest with the given string args — no server needed.
// Pass nil for handlers that take no arguments.
func makeReq(args map[string]any) mcp.CallToolRequest {
	return mcp.CallToolRequest{
		Params: mcp.CallToolParams{
			Arguments: args,
		},
	}
}

func readFileContent(targetPath string) (string, error) {
	cleanPath := filepath.Clean(targetPath)
	content, err := os.ReadFile(cleanPath)
	if err != nil {
		return "", fmt.Errorf("failed to read file: %w", err)
	}
	return string(content), nil
}

// fetchAndConvert fetches a page and converts its HTML to markdown.
//
// The client is a parameter, not a local, on purpose: the only client the server
// should ever hand out is guardedClient() (network-firewall.go), which re-checks
// the allowlist on every redirect hop and refuses an environment proxy. Keeping it
// a parameter makes that dependency visible at the call site instead of letting
// this helper quietly reach for a raw &http.Client{} again.
func fetchAndConvert(url string, client http.Client) (string, error) {
	httpReq, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return "", fmt.Errorf("invalid URL: %w", err)
	}
	httpReq.Header.Set("User-Agent", "MCP-Stdio-Server/1.0")

	resp, err := client.Do(httpReq)
	if err != nil {
		return "", fmt.Errorf("fetch failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("HTTP error: %d %s\nResponse body: %s", resp.StatusCode, resp.Status, string(body))
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("read body failed: %w", err)
	}

	converter := html2md.NewConverter("", true, nil)
	md, err := converter.ConvertString(string(body))
	if err != nil {
		return "", fmt.Errorf("html to md conversion failed: %w", err)
	}

	return strings.TrimSpace(md), nil
}
