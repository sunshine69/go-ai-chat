// godoc.go
//
// Registers three MCP tools that fetch Go documentation from pkg.go.dev,
// reusing the PlaywrightProxy already wired up in playwright.go.
//
//   fetch_godoc_package  – overview, functions, types, constants for a package
//   fetch_godoc_symbol   – a single exported symbol (func / type / method)
//   search_godoc         – full-text search across pkg.go.dev
//
// Large results (> resultSizeThreshold bytes) are written to a temp file and
// the file path is returned instead, so the AI can use the text_* tools
// (text_toc, text_grep, text_section, text_lines, …) to extract only what it needs.

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	mcp "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

const (
	pkgBase = "https://pkg.go.dev"

	// resultSizeThreshold is the byte length above which a result is written to
	// a temp file rather than returned inline.  ~8 KB keeps small lookups fast
	// while offloading large package docs.
	resultSizeThreshold = 8 * 1024
)

// ── result helpers ────────────────────────────────────────────────────────────

// maybeOffload returns the content as an inline tool result if it is small
// enough, otherwise saves it to a temp markdown file and returns the path
// together with guidance on which text_* tools to use next.
func maybeOffload(content, hint string) (*mcp.CallToolResult, error) {
	if len(content) <= resultSizeThreshold {
		return mcp.NewToolResultText(content), nil
	}

	f, err := os.CreateTemp("", "godoc-*.md")
	if err != nil {
		// Fallback: return inline even if it is large.
		return mcp.NewToolResultText(content), nil
	}
	defer f.Close()

	if _, err := f.WriteString(content); err != nil {
		return mcp.NewToolResultText(content), nil
	}

	msg := fmt.Sprintf(
		"Result is large (%d bytes) and has been saved to:\n\n%s\n\n"+
			"Suggested next steps:\n"+
			"  • text_wc      – check total size\n"+
			"  • text_toc     – list all sections (headings) in the doc\n"+
			"  • text_section – extract a specific section by heading name\n"+
			"  • text_grep    – search for a symbol or keyword\n"+
			"  • text_lines   – read a specific line range\n",
		len(content), f.Name(),
	)
	if hint != "" {
		msg += "\nHint: " + hint + "\n"
	}
	return mcp.NewToolResultText(msg), nil
}

func toolErr(format string, a ...any) *mcp.CallToolResult {
	return mcp.NewToolResultError(fmt.Sprintf(format, a...))
}

// ── registration ──────────────────────────────────────────────────────────────

// registerGoDocTools adds the three godoc tools to the MCP server.
// It reuses pw (from playwright.go) for all network requests so the
// browser session is shared and JS-rendered content is handled automatically.
func registerGoDocTools(s *server.MCPServer, pw *PlaywrightProxy) {

	// ── fetch_godoc_package ──────────────────────────────────────────────────

	s.AddTool(
		mcp.NewTool("fetch_godoc_package",
			mcp.WithDescription(
				"Fetch Go package documentation from pkg.go.dev. "+
					"Returns an overview, exported functions, types, and constants. "+
					"If the result is large it is saved to a temp file and the path is returned "+
					"so you can use text_toc / text_section / text_grep to extract what you need.",
			),
			mcp.WithString("package_path",
				mcp.Required(),
				mcp.Description(`Import path, e.g. "encoding/json" or "github.com/gin-gonic/gin"`),
			),
			mcp.WithString("version",
				mcp.Description(`Module version tag, e.g. "v1.9.1". Omit for latest.`),
			),
			mcp.WithString("section",
				mcp.Description(`Which section to return: "overview", "functions", "types", "constants", or "all" (default).`),
			),
		),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			pkgPath := req.GetString("package_path", "")
			version := req.GetString("version", "")
			section := req.GetString("section", "all")
			if pkgPath == "" {
				return toolErr("package_path is required"), nil
			}
			result, err := fetchGoDocPackage(ctx, pw, pkgPath, version, section)
			if err != nil {
				return toolErr("%v", err), nil
			}
			hint := fmt.Sprintf("use text_section with heading names like \"Functions\" or \"Types\" to drill into %s", pkgPath)
			return maybeOffload(result, hint)
		},
	)

	// ── fetch_godoc_symbol ───────────────────────────────────────────────────

	s.AddTool(
		mcp.NewTool("fetch_godoc_symbol",
			mcp.WithDescription(
				"Fetch documentation for a specific exported Go symbol "+
					"(function, type, method, or constant) from pkg.go.dev. "+
					"If the result is large it is saved to a temp file and the path is returned.",
			),
			mcp.WithString("package_path",
				mcp.Required(),
				mcp.Description(`Import path of the package, e.g. "net/http"`),
			),
			mcp.WithString("symbol",
				mcp.Required(),
				mcp.Description(`Exported symbol name, e.g. "Marshal" or "Client.Do"`),
			),
			mcp.WithString("version",
				mcp.Description(`Module version tag, e.g. "v1.9.1". Omit for latest.`),
			),
		),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			pkgPath := req.GetString("package_path", "")
			symbol := req.GetString("symbol", "")
			version := req.GetString("version", "")
			if pkgPath == "" || symbol == "" {
				return toolErr("package_path and symbol are required"), nil
			}
			result, err := fetchGoDocSymbol(ctx, pw, pkgPath, symbol, version)
			if err != nil {
				return toolErr("%v", err), nil
			}
			return maybeOffload(result, "")
		},
	)

	// ── search_godoc ─────────────────────────────────────────────────────────

	s.AddTool(
		mcp.NewTool("search_godoc",
			mcp.WithDescription(
				"Search pkg.go.dev for Go packages matching a query. "+
					"Returns package paths and synopses. "+
					"If the result is large it is saved to a temp file and the path is returned "+
					"so you can use text_grep to filter by keyword.",
			),
			mcp.WithString("query",
				mcp.Required(),
				mcp.Description(`Search keywords, e.g. "http router middleware"`),
			),
			mcp.WithNumber("limit",
				mcp.Description("Maximum number of results to return (default 10, max 25)."),
			),
		),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			query := req.GetString("query", "")
			if query == "" {
				return toolErr("query is required"), nil
			}
			limit := req.GetInt("limit", 10)
			if limit <= 0 {
				limit = 10
			}
			if limit > 25 {
				limit = 25
			}
			result, err := searchGoDocs(ctx, pw, query, limit)
			if err != nil {
				return toolErr("%v", err), nil
			}
			hint := fmt.Sprintf("use text_grep with pattern %q to filter results by keyword", query)
			return maybeOffload(result, hint)
		},
	)
}

// ── URL helper ────────────────────────────────────────────────────────────────

func pkgURL(path, version string) string {
	if version != "" {
		return fmt.Sprintf("%s/%s@%s", pkgBase, path, version)
	}
	return fmt.Sprintf("%s/%s", pkgBase, path)
}

// ── Playwright convenience wrappers ───────────────────────────────────────────

func navigate(pw *PlaywrightProxy, url string) error {
	_, err := pw.CallTool("browser_navigate", map[string]any{"url": url})
	return err
}

func evaluate(pw *PlaywrightProxy, script string) (string, error) {
	return pw.CallTool("browser_evaluate", map[string]any{"script": script})
}

// ── fetchGoDocPackage ─────────────────────────────────────────────────────────

// pkgDocResult mirrors the JSON object returned by the in-browser JS snippet.
type pkgDocResult struct {
	ImportPath string `json:"importPath"`
	Overview   string `json:"overview"`
	Functions  string `json:"functions"`
	Types      string `json:"types"`
	Constants  string `json:"constants"`
}

func fetchGoDocPackage(_ context.Context, pw *PlaywrightProxy, pkgPath, version, section string) (string, error) {
	url := pkgURL(pkgPath, version)
	if err := navigate(pw, url); err != nil {
		return "", fmt.Errorf("navigate to %s: %w", url, err)
	}

	// Single JS call; returns a proper JSON object so we can use encoding/json.
	const script = `
(function() {
  function text(sel) {
    return Array.from(document.querySelectorAll(sel))
      .map(el => el.innerText.trim())
      .filter(Boolean)
      .join('\n\n');
  }
  var repo = document.querySelector('.UnitMeta-repo a');
  return JSON.stringify({
    importPath: repo ? repo.innerText.trim() : '',
    overview:   text('.Documentation-overview p'),
    functions:  text('.Documentation-function'),
    types:      text('.Documentation-type'),
    constants:  text('.Documentation-constant'),
  });
})()`

	raw, err := evaluate(pw, script)
	if err != nil {
		return "", fmt.Errorf("evaluate: %w", err)
	}
	raw = unwrapPlaywrightString(raw)

	var doc pkgDocResult
	if err := json.Unmarshal([]byte(raw), &doc); err != nil {
		return "", fmt.Errorf("parse doc JSON: %w (raw: %.200s)", err, raw)
	}

	if doc.ImportPath == "" {
		doc.ImportPath = pkgPath
	}

	header := fmt.Sprintf("# %s\n\nimport \"%s\"\n\nSource: %s", pkgPath, doc.ImportPath, url)

	var parts []string
	if section == "overview" || section == "all" {
		if doc.Overview != "" {
			parts = append(parts, header+"\n\n"+doc.Overview)
		} else {
			parts = append(parts, header)
		}
	}
	if section == "functions" || section == "all" {
		if doc.Functions != "" {
			parts = append(parts, "## Functions\n\n"+doc.Functions)
		}
	}
	if section == "types" || section == "all" {
		if doc.Types != "" {
			parts = append(parts, "## Types\n\n"+doc.Types)
		}
	}
	if section == "constants" || section == "all" {
		if doc.Constants != "" {
			parts = append(parts, "## Constants\n\n"+doc.Constants)
		}
	}

	if len(parts) == 0 {
		return fmt.Sprintf("No documentation found for %s.", pkgPath), nil
	}
	return strings.Join(parts, "\n\n---\n\n"), nil
}

// ── fetchGoDocSymbol ──────────────────────────────────────────────────────────

type symbolResult struct {
	Found bool   `json:"found"`
	Sig   string `json:"sig"`
	Body  string `json:"body"`
}

func fetchGoDocSymbol(_ context.Context, pw *PlaywrightProxy, pkgPath, symbol, version string) (string, error) {
	url := pkgURL(pkgPath, version) + "#" + symbol
	if err := navigate(pw, url); err != nil {
		return "", fmt.Errorf("navigate to %s: %w", url, err)
	}

	script := fmt.Sprintf(`
(function() {
  var anchor = document.getElementById(%q);
  if (!anchor) return JSON.stringify({found: false, sig: '', body: ''});
  var section = anchor.closest(
    '.Documentation-function, .Documentation-type, .Documentation-method, .Documentation-constant'
  );
  if (!section) return JSON.stringify({found: false, sig: '', body: ''});
  var pre = section.querySelector('pre');
  var sig  = pre ? pre.innerText.trim() : '';
  var body = Array.from(section.querySelectorAll('p'))
               .map(function(p){ return p.innerText.trim(); })
               .filter(Boolean)
               .join('\n\n');
  return JSON.stringify({found: true, sig: sig, body: body});
})()`, symbol)

	raw, err := evaluate(pw, script)
	if err != nil {
		return "", fmt.Errorf("evaluate: %w", err)
	}
	raw = unwrapPlaywrightString(raw)

	var sym symbolResult
	if err := json.Unmarshal([]byte(raw), &sym); err != nil {
		return "", fmt.Errorf("parse symbol JSON: %w (raw: %.200s)", err, raw)
	}
	if !sym.Found {
		return fmt.Sprintf("Symbol %q not found in %s.", symbol, pkgPath), nil
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "# %s.%s\n\nSource: %s\n\n", pkgPath, symbol, url)
	if sym.Sig != "" {
		fmt.Fprintf(&sb, "```go\n%s\n```\n\n", sym.Sig)
	}
	if sym.Body != "" {
		sb.WriteString(sym.Body)
	}
	return sb.String(), nil
}

// ── searchGoDocs ──────────────────────────────────────────────────────────────

type searchEntry struct {
	Name     string `json:"name"`
	Path     string `json:"path"`
	Synopsis string `json:"synopsis"`
}

func searchGoDocs(_ context.Context, pw *PlaywrightProxy, query string, limit int) (string, error) {
	encoded := strings.NewReplacer(" ", "+", "&", "%26", "=", "%3D").Replace(query)
	url := fmt.Sprintf("%s/search?q=%s&m=package", pkgBase, encoded)
	if err := navigate(pw, url); err != nil {
		return "", fmt.Errorf("navigate to search: %w", err)
	}

	script := fmt.Sprintf(`
(function() {
  var items = Array.from(document.querySelectorAll('.SearchSnippet')).slice(0, %d);
  return JSON.stringify(items.map(function(el) {
    var link     = el.querySelector('.SearchSnippet-header a');
    var synopsis = el.querySelector('.SearchSnippet-synopsis');
    return {
      name:     link     ? link.innerText.trim()                        : '',
      path:     link     ? link.getAttribute('href').replace(/^\//, '') : '',
      synopsis: synopsis ? synopsis.innerText.trim()                    : '',
    };
  }));
})()`, limit)

	raw, err := evaluate(pw, script)
	if err != nil {
		return "", fmt.Errorf("evaluate: %w", err)
	}
	raw = unwrapPlaywrightString(raw)

	var entries []searchEntry
	if err := json.Unmarshal([]byte(raw), &entries); err != nil {
		return "", fmt.Errorf("parse search JSON: %w (raw: %.200s)", err, raw)
	}

	var lines []string
	for _, e := range entries {
		if e.Name == "" && e.Path == "" {
			continue
		}
		line := fmt.Sprintf("- **%s** (`%s`)", e.Name, e.Path)
		if e.Synopsis != "" {
			line += "\n  " + e.Synopsis
		}
		lines = append(lines, line)
	}

	if len(lines) == 0 {
		return fmt.Sprintf("No results found for %q.", query), nil
	}
	return fmt.Sprintf("# Search results for %q\n\n%s", query, strings.Join(lines, "\n\n")), nil
}

// ── unwrapPlaywrightString ────────────────────────────────────────────────────

// unwrapPlaywrightString removes the extra layer of JSON string-quoting that
// some MCP/Playwright bridges add when returning a JS value that is itself a
// JSON string.  It handles two common forms:
//
//	"{ ... }"   – outer double-quotes + inner escaped quotes
//	{ ... }     – already a bare JSON object (no unwrapping needed)
func unwrapPlaywrightString(s string) string {
	s = strings.TrimSpace(s)
	// If the value starts with a double-quote it is a JSON-encoded string;
	// unmarshal it once to get the actual JSON object text.
	if strings.HasPrefix(s, `"`) {
		var inner string
		if err := json.Unmarshal([]byte(s), &inner); err == nil {
			return inner
		}
	}
	return s
}
