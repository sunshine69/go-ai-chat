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
)

// --- MCP Protocol Structures ---

type JSONRPCRequest struct {
	Jsonrpc string          `json:"jsonrpc"`
	ID      interface{}     `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

type JSONRPCResponse struct {
	Jsonrpc string      `json:"jsonrpc"`
	ID      interface{} `json:"id"`
	Result  interface{} `json:"result,omitempty"`
	Error   *RPCError   `json:"error,omitempty"`
}

type RPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type ToolCallParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

type ToolDefinition struct {
	Name        string                 `json:"name"`
	Description string                 `json:"description"`
	InputSchema map[string]interface{} `json:"input_schema"`
}

type ListToolsResult struct {
	Tools []ToolDefinition `json:"tools"`
}

// --- Tool Logic ---

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

	var res float64
	switch args.Op {
	case "add":
		res = args.A + args.B
	case "sub":
		res = args.A - args.B
	case "mul":
		res = args.A * args.B
	case "div":
		if args.B == 0 {
			return "Error: division by zero", nil
		}
		res = args.A / args.B
	default:
		return "Error: invalid operator", nil
	}
	return fmt.Sprintf("%g", res), nil
}

type FetchArgs struct {
	URL string `json:"url"`
}

func toolFetchURL(argsRaw json.RawMessage) (string, error) {
	var args FetchArgs
	if err := json.Unmarshal(argsRaw, &args); err != nil {
		return "", err
	}
	if args.URL == "" {
		return "Error: no URL provided", nil
	}

	client := http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(args.URL)
	if err != nil {
		return "", fmt.Errorf("fetch error: %v", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1000)) // Limit to 1000 chars
	if err != nil {
		return "", err
	}

	return string(body), nil
}

// --- Server Implementation ---

func main() {
	scanner := bufio.NewScanner(os.Stdin)

	for scanner.Scan() {
		line := scanner.Text()
		if strings.TrimSpace(line) == "" {
			continue
		}

		var req JSONRPCRequest
		if err := json.Unmarshal([]byte(line), &req); err != nil {
			sendError(nil, -32700, "Parse error")
			continue
		}

		handleRequest(req)
	}
}

func handleRequest(req JSONRPCRequest) {
	var resp JSONRPCResponse
	resp.Jsonrpc = "2.0"
	resp.ID = req.ID

	switch req.Method {
	case "tools/list":
		result := ListToolsResult{
			Tools: []ToolDefinition{
				{
					Name:        "calculator",
					Description: "Perform math: add, sub, mul, div. Args: a (num), b (num), op (string)",
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
					Description: "Fetch text content from a URL.",
					InputSchema: map[string]interface{}{
						"type": "object",
						"properties": map[string]interface{}{
							"url": map[string]string{"type": "string"},
						},
						"required": []string{"url"},
					},
				},
			},
		}
		resp.Result = result

	case "tools/call":
		var callParams ToolCallParams
		if err := json.Unmarshal(req.Params, &callParams); err != nil {
			sendError(req.ID, -32602, "Invalid params")
			return
		}

		var toolRes string
		var err error

		switch callParams.Name {
		case "calculator":
			toolRes, err = toolCalculator(callParams.Arguments)
		case "fetch_url":
			toolRes, err = toolFetchURL(callParams.Arguments)
		default:
			err = fmt.Errorf("tool not found")
		}

		if err != nil {
			sendError(req.ID, -32000, err.Error())
			return
		}
		resp.Result = toolRes

	default:
		sendError(req.ID, -32601, "Method not found")
		return
	}

	sendResponse(resp)
}

func sendResponse(resp JSONRPCResponse) {
	b, _ := json.Marshal(resp)
	fmt.Println(string(b))
}

func sendError(id interface{}, code int, message string) {
	resp := JSONRPCResponse{
		Jsonrpc: "2.0",
		ID:      id,
		Error: &RPCError{
			Code:    code,
			Message: message,
		},
	}
	sendResponse(resp)
}
