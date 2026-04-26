package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ---------------------------------------------------------------------------
// JSON-RPC 2.0 types
// ---------------------------------------------------------------------------

type jsonRPCRequest struct {
	JSONRPC string      `json:"jsonrpc"`
	ID      interface{} `json:"id"`
	Method  string      `json:"method"`
	Params  interface{} `json:"params,omitempty"`
}

type jsonRPCResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      interface{}     `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *jsonRPCError   `json:"error,omitempty"`
}

type jsonRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// ---------------------------------------------------------------------------
// MCP protocol types
// ---------------------------------------------------------------------------

type mcpTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

type mcpToolsListResult struct {
	Tools []mcpTool `json:"tools"`
}

type mcpCallToolResult struct {
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	IsError bool `json:"isError,omitempty"`
}

// ---------------------------------------------------------------------------
// OpenAI tool-call types (what the LLM returns)
// ---------------------------------------------------------------------------

// ToolCall mirrors the OpenAI delta.tool_calls structure
type ToolCall struct {
	Index    int    `json:"index"`
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// ---------------------------------------------------------------------------
// MCPClient
// ---------------------------------------------------------------------------

type MCPClient struct {
	mu      sync.Mutex
	conn    io.ReadWriteCloser // TCP or pipe wrapper
	scanner *bufio.Scanner
	nextID  atomic.Int64
	tools   []mcpTool
	spec    string // original spec for display
}

// pipeReadWriter wraps an exec.Cmd's stdin/stdout into a single ReadWriteCloser
type pipeReadWriter struct {
	in  io.WriteCloser
	out io.ReadCloser
	cmd *exec.Cmd
}

func (p *pipeReadWriter) Read(b []byte) (int, error)  { return p.out.Read(b) }
func (p *pipeReadWriter) Write(b []byte) (int, error) { return p.in.Write(b) }
func (p *pipeReadWriter) Close() error {
	p.in.Close()
	p.out.Close()
	return p.cmd.Wait()
}

// ConnectTCP connects to a running MCP server over TCP.
func ConnectTCP(address string) (*MCPClient, error) {
	address = strings.TrimPrefix(address, "tcp://")
	conn, err := net.DialTimeout("tcp", address, 10*time.Second)
	if err != nil {
		return nil, fmt.Errorf("TCP connect to %s: %w", address, err)
	}
	c := &MCPClient{conn: conn, spec: "tcp://" + address}
	c.scanner = bufio.NewScanner(conn)
	c.scanner.Buffer(make([]byte, 4*1024*1024), 4*1024*1024)
	if err := c.initialize(); err != nil {
		conn.Close()
		return nil, err
	}
	return c, nil
}

// ConnectStdio launches a local MCP server process and communicates via stdio.
func ConnectStdio(cmdLine string) (*MCPClient, error) {
	parts := strings.Fields(cmdLine)
	if len(parts) == 0 {
		return nil, fmt.Errorf("empty command")
	}
	cmd := exec.Command(parts[0], parts[1:]...)
	cmd.Stderr = os.Stderr // let server errors appear in terminal

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start MCP server %q: %w", cmdLine, err)
	}

	pw := &pipeReadWriter{in: stdin, out: stdout, cmd: cmd}
	c := &MCPClient{conn: pw, spec: cmdLine}
	c.scanner = bufio.NewScanner(stdout)
	c.scanner.Buffer(make([]byte, 4*1024*1024), 4*1024*1024)
	if err := c.initialize(); err != nil {
		pw.Close()
		return nil, err
	}
	return c, nil
}

// ---------------------------------------------------------------------------
// Internal JSON-RPC helpers
// ---------------------------------------------------------------------------

func (c *MCPClient) send(req jsonRPCRequest) error {
	data, err := json.Marshal(req)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	_, err = c.conn.Write(data)
	return err
}

func (c *MCPClient) recv() (*jsonRPCResponse, error) {
	if !c.scanner.Scan() {
		if err := c.scanner.Err(); err != nil {
			return nil, err
		}
		return nil, io.EOF
	}
	line := c.scanner.Text()
	if line == "" {
		return nil, fmt.Errorf("empty line from server")
	}
	var resp jsonRPCResponse
	if err := json.Unmarshal([]byte(line), &resp); err != nil {
		return nil, fmt.Errorf("unmarshal response %q: %w", line, err)
	}
	return &resp, nil
}

func (c *MCPClient) call(method string, params interface{}) (*jsonRPCResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	id := c.nextID.Add(1)
	req := jsonRPCRequest{
		JSONRPC: "2.0",
		ID:      id,
		Method:  method,
		Params:  params,
	}
	if err := c.send(req); err != nil {
		return nil, err
	}
	// Skip notifications (no id) until we get our response
	for {
		resp, err := c.recv()
		if err != nil {
			return nil, err
		}
		if resp.ID == nil {
			continue // notification — ignore
		}
		// JSON numbers unmarshal as float64 when interface{}
		switch v := resp.ID.(type) {
		case float64:
			if int64(v) == id {
				return resp, nil
			}
		case int64:
			if v == id {
				return resp, nil
			}
		}
		// Different ID — unexpected; skip
	}
}

// ---------------------------------------------------------------------------
// MCP protocol handshake
// ---------------------------------------------------------------------------

func (c *MCPClient) initialize() error {
	params := map[string]interface{}{
		"protocolVersion": "2024-11-05",
		"capabilities":    map[string]interface{}{},
		"clientInfo": map[string]interface{}{
			"name":    "aig",
			"version": "1.0.0",
		},
	}
	resp, err := c.call("initialize", params)
	if err != nil {
		return fmt.Errorf("initialize: %w", err)
	}
	if resp.Error != nil {
		return fmt.Errorf("initialize error: %s", resp.Error.Message)
	}

	// Send initialized notification (no response expected)
	c.mu.Lock()
	_ = c.send(jsonRPCRequest{
		JSONRPC: "2.0",
		Method:  "notifications/initialized",
	})
	c.mu.Unlock()

	// Fetch tool list
	return c.refreshTools()
}

func (c *MCPClient) refreshTools() error {
	resp, err := c.call("tools/list", nil)
	if err != nil {
		return fmt.Errorf("tools/list: %w", err)
	}
	if resp.Error != nil {
		return fmt.Errorf("tools/list error: %s", resp.Error.Message)
	}
	var result mcpToolsListResult
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		return fmt.Errorf("parse tools: %w", err)
	}
	c.tools = result.Tools
	return nil
}

// CallTool invokes a named MCP tool with the given arguments (JSON-encoded map).
func (c *MCPClient) CallTool(name string, arguments map[string]interface{}) (string, error) {
	params := map[string]interface{}{
		"name":      name,
		"arguments": arguments,
	}
	resp, err := c.call("tools/call", params)
	if err != nil {
		return "", fmt.Errorf("tools/call %s: %w", name, err)
	}
	if resp.Error != nil {
		return "", fmt.Errorf("tool %s error: %s", name, resp.Error.Message)
	}
	var result mcpCallToolResult
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		return "", fmt.Errorf("parse tool result: %w", err)
	}
	var parts []string
	for _, c := range result.Content {
		if c.Type == "text" {
			parts = append(parts, c.Text)
		}
	}
	out := strings.Join(parts, "\n")
	if result.IsError {
		return "", fmt.Errorf("tool error: %s", out)
	}
	return out, nil
}

// Tools returns the list of available MCP tools.
func (c *MCPClient) Tools() []mcpTool { return c.tools }

// Close shuts down the MCP connection.
func (c *MCPClient) Close() error { return c.conn.Close() }

// ---------------------------------------------------------------------------
// OpenAI "tools" schema conversion
// ---------------------------------------------------------------------------

// ToOpenAITools converts MCP tool descriptors to OpenAI-compatible tool specs.
func ToOpenAITools(tools []mcpTool) []map[string]interface{} {
	out := make([]map[string]interface{}, 0, len(tools))
	for _, t := range tools {
		var schema interface{}
		if len(t.InputSchema) > 0 {
			_ = json.Unmarshal(t.InputSchema, &schema)
		}
		if schema == nil {
			schema = map[string]interface{}{"type": "object", "properties": map[string]interface{}{}}
		}
		out = append(out, map[string]interface{}{
			"type": "function",
			"function": map[string]interface{}{
				"name":        t.Name,
				"description": t.Description,
				"parameters":  schema,
			},
		})
	}
	return out
}
