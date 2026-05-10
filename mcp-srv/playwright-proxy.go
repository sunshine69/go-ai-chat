// playwright.go

package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
)

type PlaywrightProxy struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout *bufio.Reader
	mu     sync.Mutex
	nextID atomic.Int64
}

type jsonRPCRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int64  `json:"id"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

type jsonRPCResponse struct {
	ID     int64           `json:"id"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *struct {
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

func NewPlaywrightProxy() (*PlaywrightProxy, error) {
	cmd := exec.Command("npx", "@playwright/mcp@latest", "--headless")

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("stdin pipe: %w", err)
	}
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start playwright mcp: %w", err)
	}

	p := &PlaywrightProxy{
		cmd:    cmd,
		stdin:  stdin,
		stdout: bufio.NewReader(stdoutPipe),
	}

	// Send initialize handshake
	if err := p.initialize(); err != nil {
		cmd.Process.Kill()
		return nil, fmt.Errorf("initialize: %w", err)
	}

	return p, nil
}

func (p *PlaywrightProxy) call(method string, params any) (json.RawMessage, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	id := p.nextID.Add(1)
	req := jsonRPCRequest{
		JSONRPC: "2.0",
		ID:      id,
		Method:  method,
		Params:  params,
	}

	line, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	if _, err := fmt.Fprintf(p.stdin, "%s\n", line); err != nil {
		return nil, fmt.Errorf("write: %w", err)
	}

	// Read until we get the matching response ID
	for {
		respLine, err := p.stdout.ReadString('\n')
		if err != nil {
			return nil, fmt.Errorf("read: %w", err)
		}
		respLine = strings.TrimSpace(respLine)
		if respLine == "" {
			continue
		}

		var resp jsonRPCResponse
		if err := json.Unmarshal([]byte(respLine), &resp); err != nil {
			continue // skip non-JSON lines (e.g. stderr mixed in)
		}
		if resp.ID != id {
			continue // response for a different request
		}
		if resp.Error != nil {
			return nil, fmt.Errorf("playwright error: %s", resp.Error.Message)
		}
		return resp.Result, nil
	}
}

func (p *PlaywrightProxy) initialize() error {
	_, err := p.call("initialize", map[string]any{
		"protocolVersion": "2024-11-05",
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "mcp-proxy", "version": "1.0"},
	})
	if err != nil {
		return err
	}
	// Send initialized notification (no response expected)
	p.mu.Lock()
	defer p.mu.Unlock()
	notif, _ := json.Marshal(jsonRPCRequest{
		JSONRPC: "2.0",
		Method:  "notifications/initialized",
	})
	fmt.Fprintf(p.stdin, "%s\n", notif)
	return nil
}

func (p *PlaywrightProxy) CallTool(name string, args map[string]any) (string, error) {
	result, err := p.call("tools/call", map[string]any{
		"name":      name,
		"arguments": args,
	})
	if err != nil {
		return "", err
	}

	// Extract text from content array
	var parsed struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(result, &parsed); err != nil {
		return string(result), nil
	}
	var parts []string
	for _, c := range parsed.Content {
		if c.Text != "" {
			parts = append(parts, c.Text)
		}
	}
	return strings.Join(parts, "\n"), nil
}

func (p *PlaywrightProxy) Close() {
	p.stdin.Close()
	p.cmd.Wait()
}
