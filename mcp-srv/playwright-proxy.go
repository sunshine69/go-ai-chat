// playwright.go

package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"

	mcp "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// playwrightMCPVersion pins the upstream @playwright/mcp package instead of
// riding @latest. Tool names/params (ref vs target, browser_evaluate's code
// param, etc.) have changed across releases before, and an unpinned version
// can silently break this proxy on the next `npx` invocation. Bump this
// deliberately after testing against the new version.
const playwrightMCPVersion = "0.0.77"

type PlaywrightProxy struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout *bufio.Reader
	mu     sync.Mutex
	nextID atomic.Int64

	// evalOnce/evalParamName/evalWrapInPageFn/evalErr cache the resolved
	// shape of the upstream browser_evaluate tool (see evaluateSchema).
	// Resolved lazily on first use rather than hardcoded, since this param
	// name/semantics have changed across @playwright/mcp releases before.
	evalOnce         sync.Once
	evalParamName    string
	evalWrapInPageFn bool
	evalRawSchema    json.RawMessage
	evalErr          error
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

// rpcTool mirrors the shape of a single entry returned by the upstream
// server's tools/list response.
type rpcTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

// registerPlaywrightTools discovers the real tool set from the running
// @playwright/mcp process via tools/list and registers each one with its
// actual upstream JSON schema. This intentionally avoids hand-maintaining
// tool definitions: names and parameters (e.g. browser_click's "ref",
// browser_evaluate's "code") have changed across releases, and copying the
// real schema through verbatim is the only way to stay correct without
// re-auditing this file on every bump of playwrightMCPVersion.
func registerPlaywrightTools(s *server.MCPServer, pw *PlaywrightProxy) error {
	tools, err := pw.ListTools()
	if err != nil {
		return fmt.Errorf("listing playwright tools: %w", err)
	}
	if len(tools) == 0 {
		return fmt.Errorf("playwright mcp reported zero tools (version pinned to %s — check it started correctly)", playwrightMCPVersion)
	}

	for _, t := range tools {
		toolName := t.Name
		schema := t.InputSchema
		if len(schema) == 0 {
			// Some tools (e.g. browser_snapshot) take no parameters and may
			// omit inputSchema entirely. Fall back to an empty object schema
			// so mcp-go doesn't choke on a nil RawInputSchema.
			schema = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		tool := mcp.NewToolWithRawSchema(toolName, t.Description, schema)
		s.AddTool(tool, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			result, err := pw.CallTool(toolName, req.GetArguments())
			if err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
			return mcp.NewToolResultText(result), nil
		})
	}

	return nil
}

func NewPlaywrightProxy() (*PlaywrightProxy, error) {
	cmd := exec.Command("npx", fmt.Sprintf("@playwright/mcp@%s", playwrightMCPVersion), "--headless")

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

// ListTools calls tools/list on the upstream server and returns the raw
// tool definitions (name, description, JSON schema) as reported right now
// by the pinned playwrightMCPVersion process. Used both for dynamic
// registration in registerPlaywrightTools and available for ad-hoc
// diagnostics (e.g. logging discovered tools at startup).
func (p *PlaywrightProxy) ListTools() ([]rpcTool, error) {
	result, err := p.call("tools/list", map[string]any{})
	if err != nil {
		return nil, err
	}
	var parsed struct {
		Tools []rpcTool `json:"tools"`
	}
	if err := json.Unmarshal(result, &parsed); err != nil {
		return nil, fmt.Errorf("unmarshal tools/list: %w", err)
	}
	return parsed.Tools, nil
}

// evaluateSchema resolves, once, which parameter browser_evaluate actually
// expects and whether its value should be an "async (page) => {...}"
// wrapper (Playwright-side function receiving `page`) or a bare script.
// This is read from the live tools/list response rather than assumed,
// because both the field name (seen as "code", "function", "expression"
// across releases) and the calling convention have changed between
// @playwright/mcp versions.
func (p *PlaywrightProxy) evaluateSchema() (paramName string, wrapInPageFn bool, err error) {
	p.evalOnce.Do(func() {
		tools, listErr := p.ListTools()
		if listErr != nil {
			p.evalErr = fmt.Errorf("resolving browser_evaluate schema: %w", listErr)
			return
		}
		for _, t := range tools {
			if t.Name != "browser_evaluate" {
				continue
			}
			var schema struct {
				Properties map[string]struct {
					Type        string `json:"type"`
					Description string `json:"description"`
				} `json:"properties"`
				Required []string `json:"required"`
			}
			if err := json.Unmarshal(t.InputSchema, &schema); err != nil {
				p.evalErr = fmt.Errorf("parsing browser_evaluate schema: %w", err)
				return
			}

			name := ""
			if len(schema.Required) > 0 {
				name = schema.Required[0]
			} else {
				for _, candidate := range []string{"function", "code", "script", "expression"} {
					if prop, ok := schema.Properties[candidate]; ok && prop.Type == "string" {
						name = candidate
						break
					}
				}
			}
			if name == "" {
				p.evalErr = fmt.Errorf("could not determine browser_evaluate's script parameter from schema: %s", string(t.InputSchema))
				return
			}
			p.evalParamName = name
			p.evalRawSchema = t.InputSchema

			desc := strings.ToLower(schema.Properties[name].Description)
			p.evalWrapInPageFn = strings.Contains(desc, "argument, page") ||
				strings.Contains(desc, "(page)") ||
				strings.Contains(desc, "playwright code")
			return
		}
		p.evalErr = fmt.Errorf("browser_evaluate tool not found in tools/list (is it enabled for this --caps set?)")
	})
	return p.evalParamName, p.evalWrapInPageFn, p.evalErr
}

// evalSchemaDebug returns the raw JSON schema browser_evaluate reported at
// resolution time, for inclusion in error messages when a call still fails
// despite evaluateSchema's best guess at the param name/convention.
func (p *PlaywrightProxy) evalSchemaDebug() string {
	if len(p.evalRawSchema) == 0 {
		return "(schema not resolved)"
	}
	return string(p.evalRawSchema)
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
