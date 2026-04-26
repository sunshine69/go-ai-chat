package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strings"

	html2md "github.com/JohannesKaufmann/html-to-markdown"
)

// =============================================================================
// JSON-RPC 2.0 & MCP Structures
// =============================================================================

type Request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      interface{}     `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type Response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      interface{}     `json:"id"`
	Result  *ResponseResult `json:"result,omitempty"`
	Error   *ErrorResponse  `json:"error,omitempty"`
}

type ResponseResult struct {
	ProtocolVersion string                 `json:"protocolVersion,omitempty"`
	Capabilities    map[string]interface{} `json:"capabilities,omitempty"`
	ServerInfo      map[string]string      `json:"serverInfo,omitempty"`
	Tools           []Tool                 `json:"tools,omitempty"`
	Content         []ContentBlock         `json:"content,omitempty"`
}

type ErrorResponse struct {
	Code    int         `json:"code"`
	Message string      `json:"message"`
	Data    interface{} `json:"data,omitempty"`
}

// MCP Tool Definition
type Tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

// MCP Content Block
type ContentBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

const mcpVersion = "2024-11-05"

// =============================================================================
// Core Server Logic
// =============================================================================

func main() {
	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		var req Request
		if err := json.Unmarshal(line, &req); err != nil {
			writeResponse(req.ID, nil, &ErrorResponse{Code: -32700, Message: "Parse error"})
			continue
		}

		switch req.Method {
		case "initialize":
			handleInitialize(&req)
		case "tools/list":
			handleToolsList(&req)
		case "tools/call":
			handleToolCall(&req)
		default:
			writeResponse(req.ID, nil, &ErrorResponse{Code: -32601, Message: "Method not found"})
		}
	}

	if err := scanner.Err(); err != nil {
		fmt.Fprintf(os.Stderr, "stdin error: %v\n", err)
		os.Exit(1)
	}
}

func writeResponse(id interface{}, result *ResponseResult, err *ErrorResponse) {
	resp := Response{
		JSONRPC: "2.0",
		ID:      id,
	}
	if result != nil {
		resp.Result = result
	} else {
		resp.Error = err
	}

	out, marshalErr := json.Marshal(resp)
	if marshalErr != nil {
		fmt.Fprintf(os.Stderr, "marshal error: %v\n", marshalErr)
		return
	}
	fmt.Fprintln(os.Stdout, string(out))
}

func handleInitialize(req *Request) {
	writeResponse(req.ID, &ResponseResult{
		ProtocolVersion: mcpVersion,
		Capabilities:    map[string]interface{}{"tools": struct{}{}},
		ServerInfo:      map[string]string{"name": "mcp-fetch-server", "version": "1.0.0"},
	}, nil)
}

func handleToolsList(req *Request) {
	schema := json.RawMessage(`{
		"type": "object",
		"properties": {
			"url": {
				"type": "string",
				"description": "The URL to fetch and convert into markdown text"
			}
		},
		"required": ["url"]
	}`)

	writeResponse(req.ID, &ResponseResult{
		Tools: []Tool{{
			Name:        "fetch_url",
			Description: "Fetches a URL over HTTP and converts its HTML content into markdown text.",
			InputSchema: schema,
		}},
	}, nil)
}

func handleToolCall(req *Request) {
	var args struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments,omitempty"`
	}
	if err := json.Unmarshal(req.Params, &args); err != nil {
		writeResponse(req.ID, nil, &ErrorResponse{Code: -32602, Message: "Invalid params"})
		return
	}

	if args.Name == "fetch_url" {
		var params struct {
			URL string `json:"url"`
		}
		if err := json.Unmarshal(args.Arguments, &params); err != nil || strings.TrimSpace(params.URL) == "" {
			writeResponse(req.ID, nil, &ErrorResponse{Code: -32602, Message: "Missing or invalid 'url' parameter"})
			return
		}

		markdownText, err := fetchAndConvert(params.URL)
		if err != nil {
			writeResponse(req.ID, nil, &ErrorResponse{Code: -32603, Message: err.Error()})
			return
		}

		writeResponse(req.ID, &ResponseResult{
			Content: []ContentBlock{{Type: "text", Text: markdownText}},
		}, nil)
	} else {
		writeResponse(req.ID, nil, &ErrorResponse{Code: -32601, Message: fmt.Sprintf("Tool '%s' not found", args.Name)})
	}
}

// =============================================================================
// Tool Implementation (stdlib-only HTML -> Markdown)
// =============================================================================

func fetchAndConvert(url string) (string, error) {
	client := &http.Client{}
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
		return "", fmt.Errorf("HTTP error: %d %s", resp.StatusCode, resp.Status)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("read body failed: %w", err)
	}

	return HTMLToMarkdown(body)
}

func HTMLToMarkdown(htmlb []byte) (string, error) {
	html := string(htmlb)
	if strings.TrimSpace(html) == "" {
		return "", nil
	}

	converter := html2md.NewConverter("", true, nil)
	md, err := converter.ConvertString(html)
	if err != nil {
		return "", fmt.Errorf("html to md: %w", err)
	}

	return strings.TrimSpace(md), nil
}

// htmlToMarkdown converts raw HTML to a simplified markdown format.
// NOTE: Full HTML-to-markdown parsing typically requires external libraries
// (e.g., github.com/JohannesKaufmann/html-to-markdown). This stdlib-only
// implementation strips tags, preserves block structure, and collapses whitespace.
func htmlToMarkdown(html []byte) string {
	text := string(html)

	// Remove script & style blocks entirely
	reBlock := regexp.MustCompile(`(?is)<script[^>]*>.*?</script>|<style[^>]*>.*?</style>`)
	text = reBlock.ReplaceAllString(text, "")

	// Strip all remaining HTML tags
	reTag := regexp.MustCompile(`</?[^>]+>`)
	text = reTag.ReplaceAllString(text, " ")

	// Convert block elements to newlines for markdown structure
	blockRe := regexp.MustCompile(`<br\s*/?>|<p>|</p>|<div>|</div>|<h[1-6]>|</h[1-6]>`)
	text = blockRe.ReplaceAllString(text, "\n")

	// Collapse multiple whitespace/newlines into single spaces
	reWS := regexp.MustCompile(`\s+`)
	text = reWS.ReplaceAllString(text, " ")

	return strings.TrimSpace(text)
}
