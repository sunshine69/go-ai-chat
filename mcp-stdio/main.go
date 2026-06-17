package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	md "github.com/JohannesKaufmann/html-to-markdown"
)

// ---------- JSON-RPC base ----------

type JSONRPCRequest struct {
	Jsonrpc string          `json:"jsonrpc"`
	ID      interface{}     `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type JSONRPCResponse struct {
	Jsonrpc string      `json:"jsonrpc"`
	ID      interface{} `json:"id,omitempty"`
	Result  interface{} `json:"result,omitempty"`
	Error   *RPCError   `json:"error,omitempty"`
}

type RPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// ---------- MCP Types ----------

type Tool struct {
	Name        string                 `json:"name"`
	Description string                 `json:"description"`
	InputSchema map[string]interface{} `json:"inputSchema"` // was "input_schema" — MCP spec requires camelCase
}

type ToolCallParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

// ---------- Tool Implementations ----------

type CalculatorArgs struct {
	A  float64 `json:"a"`
	B  float64 `json:"b"`
	Op string  `json:"op"`
}

func toolCalculator(argsRaw json.RawMessage) (string, error) {
	var args CalculatorArgs
	if err := json.Unmarshal(argsRaw, &args); err != nil {
		return "", err
	}

	switch args.Op {
	case "add":
		return fmt.Sprintf("%g", args.A+args.B), nil
	case "sub":
		return fmt.Sprintf("%g", args.A-args.B), nil
	case "mul":
		return fmt.Sprintf("%g", args.A*args.B), nil
	case "div":
		if args.B == 0 {
			return "Error: division by zero", nil
		}
		return fmt.Sprintf("%g", args.A/args.B), nil
	default:
		return "Error: invalid operator", nil
	}
}

type FetchArgs struct {
	URL string `json:"url"`
}

func toolFetch(argsRaw json.RawMessage) (string, error) {
	var args FetchArgs
	if err := json.Unmarshal(argsRaw, &args); err != nil {
		return "", err
	}

	if args.URL == "" {
		return "Error: missing URL", nil
	}

	// Use a client with timeout
	client := &http.Client{Timeout: 8 * time.Second}
	req, err := http.NewRequest("GET", args.URL, nil)
	if err != nil {
		return "", err
	}

	// Set a user-agent (many sites require it)
	req.Header.Set("User-Agent", "Go-MCP-Server/1.0 (+https://example.com)")

	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	// Read body
	body, err := io.ReadAll(io.LimitReader(resp.Body, 3000000)) // limit to ~3MB
	if err != nil {
		return "", err
	}

	// Convert HTML to Markdown
	md := md.NewConverter().Convert(string(body))
	if err != nil {
		// Optionally: log the error or fallback to raw HTML
		return string(body), fmt.Errorf("HTML→MD conversion failed: %w", err)
	}

	return md, nil
}

// ---------- Server ----------

func main() {
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 0, 1024), 1024*1024)

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}

		var req JSONRPCRequest
		if err := json.Unmarshal([]byte(line), &req); err != nil {
			sendError(nil, -32700, "Parse error")
			continue
		}

		handle(req)
	}
}

// ---------- Handler ----------

func handle(req JSONRPCRequest) {
	switch req.Method {

	case "initialize":
		sendResult(req.ID, map[string]interface{}{
			"protocolVersion": "2024-11-05",
			"capabilities": map[string]interface{}{
				"tools": map[string]interface{}{},
			},
			"serverInfo": map[string]string{
				"name":    "go-mcp-minimal",
				"version": "1.0",
			},
		})

	case "notifications/initialized", "initialized":
		// notification — no response

	case "tools/list":
		sendResult(req.ID, map[string]interface{}{
			"tools": []Tool{
				{
					Name:        "calculator",
					Description: "Basic math: add, sub, mul, div",
					InputSchema: map[string]interface{}{
						"type": "object",
						"properties": map[string]interface{}{
							"a":  map[string]string{"type": "number"},
							"b":  map[string]string{"type": "number"},
							"op": map[string]interface{}{"type": "string", "enum": []string{"add", "sub", "mul", "div"}},
						},
						"required": []string{"a", "b", "op"},
					},
				},
				{
					Name:        "fetch_url",
					Description: "Fetch text from a URL (first 1000 bytes)",
					InputSchema: map[string]interface{}{
						"type": "object",
						"properties": map[string]interface{}{
							"url": map[string]string{"type": "string"},
						},
						"required": []string{"url"},
					},
				},
			},
		})

	case "tools/call":
		var p ToolCallParams
		if err := json.Unmarshal(req.Params, &p); err != nil {
			sendError(req.ID, -32602, "Invalid params")
			return
		}

		var (
			out string
			err error
		)

		switch p.Name {
		case "calculator":
			out, err = toolCalculator(p.Arguments)
		case "fetch_url":
			out, err = toolFetch(p.Arguments)
		default:
			sendError(req.ID, -32601, "Tool not found")
			return
		}

		if err != nil {
			sendError(req.ID, -32000, err.Error())
			return
		}

		sendResult(req.ID, map[string]interface{}{
			"content": []map[string]string{
				{"type": "text", "text": out},
			},
		})

	case "shutdown":
		sendResult(req.ID, nil)

	case "exit":
		os.Exit(0)

	default:
		sendError(req.ID, -32601, "Method not found")
	}
}

// ---------- Helpers ----------

func sendResult(id interface{}, result interface{}) {
	write(JSONRPCResponse{Jsonrpc: "2.0", ID: id, Result: result})
}

func sendError(id interface{}, code int, msg string) {
	write(JSONRPCResponse{Jsonrpc: "2.0", ID: id, Error: &RPCError{Code: code, Message: msg}})
}

func write(v interface{}) {
	b, _ := json.Marshal(v)
	fmt.Fprintln(os.Stderr, string(b))
}
