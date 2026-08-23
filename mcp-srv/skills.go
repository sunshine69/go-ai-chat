// skills.go

package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"

	mcp "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// skillsMCPVersion pins the upstream @gengirish/skills-mcp package instead
// of riding @latest, for the same reason playwrightMCPVersion is pinned:
// tool names/params can change across releases and an unpinned version can
// silently break this proxy on the next `npx` invocation. Bump this
// deliberately after testing against the new version.
const skillsMCPVersion = "1.0.0"

// SkillsProxy speaks JSON-RPC 2.0 over stdio to a spawned
// `npx @gengirish/skills-mcp` process. It is structurally identical to
// PlaywrightProxy (see playwright.go) — both wrap a stdio MCP server — but
// kept as a separate type/process since each upstream is a distinct child
// process with its own lifecycle.
type SkillsProxy struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout *bufio.Reader
	mu     sync.Mutex
	nextID atomic.Int64
}

// registerSkillsTools discovers the real tool set from the running
// @gengirish/skills-mcp process via tools/list and registers each one with
// its actual upstream JSON schema, for the same reason playwright's tools
// are registered dynamically rather than hand-copied: the upstream repo's
// tool set (search_skills, get_skill, recommend_skills, list_domains,
// list_repos, install_skill, catalog_stats, ...) can change across releases
// without this proxy needing a matching code change beyond a version bump.
func registerSkillsTools(s *server.MCPServer, sk *SkillsProxy) error {
	tools, err := sk.ListTools()
	if err != nil {
		return fmt.Errorf("listing skills-mcp tools: %w", err)
	}
	if len(tools) == 0 {
		return fmt.Errorf("skills-mcp reported zero tools (version pinned to %s — check it started correctly)", skillsMCPVersion)
	}

	for _, t := range tools {
		toolName := t.Name
		schema := t.InputSchema
		if len(schema) == 0 {
			schema = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		tool := mcp.NewToolWithRawSchema(toolName, t.Description, schema)
		s.AddTool(tool, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			result, err := sk.CallTool(toolName, req.GetArguments())
			if err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
			return mcp.NewToolResultText(result), nil
		})
	}

	return nil
}

// NewSkillsProxy spawns `npx @gengirish/skills-mcp@<version>` and completes
// the MCP initialize handshake. If GITHUB_TOKEN is set in this process's
// environment, it is forwarded to the child so get_skill/install_skill get
// the authenticated 5,000 req/hr GitHub API rate limit instead of the
// anonymous 60 req/hr limit (see the "Optional: avoid GitHub rate limits"
// section of the upstream README).
func NewSkillsProxy() (*SkillsProxy, error) {
	cmd := exec.Command("npx", "-y", fmt.Sprintf("@gengirish/skills-mcp@%s", skillsMCPVersion))
	cmd.Env = os.Environ()

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("stdin pipe: %w", err)
	}
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start skills-mcp: %w", err)
	}

	sk := &SkillsProxy{
		cmd:    cmd,
		stdin:  stdin,
		stdout: bufio.NewReader(stdoutPipe),
	}

	if err := sk.initialize(); err != nil {
		cmd.Process.Kill()
		return nil, fmt.Errorf("initialize: %w", err)
	}

	return sk, nil
}

func (sk *SkillsProxy) call(method string, params any) (json.RawMessage, error) {
	sk.mu.Lock()
	defer sk.mu.Unlock()

	id := sk.nextID.Add(1)
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
	if _, err := fmt.Fprintf(sk.stdin, "%s\n", line); err != nil {
		return nil, fmt.Errorf("write: %w", err)
	}

	for {
		respLine, err := sk.stdout.ReadString('\n')
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
			return nil, fmt.Errorf("skills-mcp error: %s", resp.Error.Message)
		}
		return resp.Result, nil
	}
}

func (sk *SkillsProxy) initialize() error {
	_, err := sk.call("initialize", map[string]any{
		"protocolVersion": "2024-11-05",
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "mcp-proxy", "version": "1.0"},
	})
	if err != nil {
		return err
	}
	sk.mu.Lock()
	defer sk.mu.Unlock()
	notif, _ := json.Marshal(jsonRPCRequest{
		JSONRPC: "2.0",
		Method:  "notifications/initialized",
	})
	fmt.Fprintf(sk.stdin, "%s\n", notif)
	return nil
}

// ListTools calls tools/list on the upstream server and returns the raw
// tool definitions (name, description, JSON schema) as reported right now
// by the pinned skillsMCPVersion process.
func (sk *SkillsProxy) ListTools() ([]rpcTool, error) {
	result, err := sk.call("tools/list", map[string]any{})
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

func (sk *SkillsProxy) CallTool(name string, args map[string]any) (string, error) {
	result, err := sk.call("tools/call", map[string]any{
		"name":      name,
		"arguments": args,
	})
	if err != nil {
		return "", err
	}

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

func (sk *SkillsProxy) Close() {
	sk.stdin.Close()
	sk.cmd.Wait()
}
