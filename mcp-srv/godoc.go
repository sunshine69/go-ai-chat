// godoc.go
//
// Registers three MCP tools that fetch Go documentation from pkg.go.dev,
// reusing the PlaywrightProxy already wired up in playwright.go.
//
//   fetch_godoc_package  – overview, functions, types, constants for a package
//   fetch_godoc_symbol   – a single exported symbol (func / type / method)
//   search_godoc         – full-text search across pkg.go.dev

package main

import (
	"context"
	"fmt"
	"strings"

	mcp "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

const pkgBase = "https://pkg.go.dev"

// registerGoDocTools adds the three godoc tools to the MCP server.
// It reuses pw (from playwright.go) for all network requests so the
// browser session is shared and JS-rendered content is handled automatically.
func registerGoDocTools(s *server.MCPServer, pw *PlaywrightProxy) {

	// ── fetch_godoc_package ────────────────────────────────────────────

	s.AddTool(
		mcp.NewTool("fetch_godoc_package",
			mcp.WithDescription("Fetch Go package documentation from pkg.go.dev. "+
				"Returns an overview, exported functions, types, and constants."),
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
			args := req.GetArguments()
			pkgPath, _ := args["package_path"].(string)
			version, _ := args["version"].(string)
			section, _ := args["section"].(string)
			if section == "" {
				section = "all"
			}
			if pkgPath == "" {
				return mcp.NewToolResultError("package_path is required"), nil
			}

			result, err := fetchGoDocPackage(ctx, pw, pkgPath, version, section)
			if err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
			return mcp.NewToolResultText(result), nil
		},
	)

	// ── fetch_godoc_symbol ─────────────────────────────────────────────

	s.AddTool(
		mcp.NewTool("fetch_godoc_symbol",
			mcp.WithDescription("Fetch documentation for a specific exported Go symbol "+
				"(function, type, method, or constant) from pkg.go.dev."),
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
			args := req.GetArguments()
			pkgPath, _ := args["package_path"].(string)
			symbol, _ := args["symbol"].(string)
			version, _ := args["version"].(string)
			if pkgPath == "" || symbol == "" {
				return mcp.NewToolResultError("package_path and symbol are required"), nil
			}

			result, err := fetchGoDocSymbol(ctx, pw, pkgPath, symbol, version)
			if err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
			return mcp.NewToolResultText(result), nil
		},
	)

	// ── search_godoc ───────────────────────────────────────────────────

	s.AddTool(
		mcp.NewTool("search_godoc",
			mcp.WithDescription("Search pkg.go.dev for Go packages matching a query. "+
				"Returns package paths and synopses."),
			mcp.WithString("query",
				mcp.Required(),
				mcp.Description(`Search keywords, e.g. "http router middleware"`),
			),
			mcp.WithNumber("limit",
				mcp.Description("Maximum number of results to return (default 10, max 25)."),
			),
		),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			args := req.GetArguments()
			query, _ := args["query"].(string)
			limit := 10
			if l, ok := args["limit"].(float64); ok && l > 0 {
				limit = int(l)
				if limit > 25 {
					limit = 25
				}
			}
			if query == "" {
				return mcp.NewToolResultError("query is required"), nil
			}

			result, err := searchGoDocs(ctx, pw, query, limit)
			if err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
			return mcp.NewToolResultText(result), nil
		},
	)
}

// ── implementation helpers ─────────────────────────────────────────────────
//
// All three helpers follow the same pattern:
//   1. Navigate Playwright to the target URL.
//   2. Use browser_evaluate to extract structured text from the DOM.
//   3. Return the result as a Markdown string.
//
// Playwright is used instead of raw HTTP because pkg.go.dev renders some
// content client-side (symbol filtering, version switching).

func pkgURL(path, version string) string {
	if version != "" {
		return fmt.Sprintf("%s/%s@%s", pkgBase, path, version)
	}
	return fmt.Sprintf("%s/%s", pkgBase, path)
}

// navigate navigates the Playwright browser to url and returns any error.
func navigate(pw *PlaywrightProxy, url string) error {
	_, err := pw.CallTool("browser_navigate", map[string]any{"url": url})
	return err
}

// evaluate runs a JS snippet in the browser and returns its string result.
func evaluate(pw *PlaywrightProxy, script string) (string, error) {
	return pw.CallTool("browser_evaluate", map[string]any{"script": script})
}

// fetchGoDocPackage fetches package-level documentation.
func fetchGoDocPackage(_ context.Context, pw *PlaywrightProxy, pkgPath, version, section string) (string, error) {
	url := pkgURL(pkgPath, version)
	if err := navigate(pw, url); err != nil {
		return "", fmt.Errorf("navigate to %s: %w", url, err)
	}

	// One JS call extracts every section we might need.
	// We return a JSON-ish block of labelled sections and slice client-side.
	script := `
(function() {
  function text(sel) {
    return Array.from(document.querySelectorAll(sel))
      .map(el => el.innerText.trim())
      .filter(Boolean)
      .join('\n\n');
  }
  return JSON.stringify({
    importPath: (document.querySelector('.UnitMeta-repo a') || {}).innerText || '',
    overview:   text('.Documentation-overview p'),
    functions:  text('.Documentation-function'),
    types:      text('.Documentation-type'),
    constants:  text('.Documentation-constant'),
  });
})()
`
	raw, err := evaluate(pw, script)
	if err != nil {
		return "", fmt.Errorf("evaluate: %w", err)
	}

	// raw is a JSON string (Playwright wraps the return value in quotes).
	// Strip surrounding quotes added by some MCP bridges.
	raw = strings.Trim(raw, `"`)
	raw = strings.ReplaceAll(raw, `\"`, `"`)

	// Simple manual decode – avoids a json import and keeps things flat.
	get := func(key string) string {
		marker := `"` + key + `":"`
		start := strings.Index(raw, marker)
		if start < 0 {
			return ""
		}
		start += len(marker)
		end := start
		for end < len(raw) {
			if raw[end] == '"' && raw[end-1] != '\\' {
				break
			}
			end++
		}
		val := raw[start:end]
		return strings.ReplaceAll(val, `\n`, "\n")
	}

	importPath := get("importPath")
	if importPath == "" {
		importPath = pkgPath
	}

	var parts []string
	header := fmt.Sprintf("# %s\n\nimport \"%s\"", pkgPath, importPath)

	if section == "overview" || section == "all" {
		if ov := get("overview"); ov != "" {
			parts = append(parts, header+"\n\n"+ov)
		} else {
			parts = append(parts, header)
		}
	}
	if section == "functions" || section == "all" {
		if fn := get("functions"); fn != "" {
			parts = append(parts, "## Functions\n\n"+fn)
		}
	}
	if section == "types" || section == "all" {
		if ty := get("types"); ty != "" {
			parts = append(parts, "## Types\n\n"+ty)
		}
	}
	if section == "constants" || section == "all" {
		if co := get("constants"); co != "" {
			parts = append(parts, "## Constants\n\n"+co)
		}
	}

	if len(parts) == 0 {
		return fmt.Sprintf("No documentation found for %s.", pkgPath), nil
	}
	return strings.Join(parts, "\n\n---\n\n"), nil
}

// fetchGoDocSymbol fetches docs for a single exported symbol.
func fetchGoDocSymbol(_ context.Context, pw *PlaywrightProxy, pkgPath, symbol, version string) (string, error) {
	url := pkgURL(pkgPath, version) + "#" + symbol
	if err := navigate(pw, url); err != nil {
		return "", fmt.Errorf("navigate to %s: %w", url, err)
	}

	script := fmt.Sprintf(`
(function() {
  // Symbols are anchored by id matching the symbol name.
  var anchor = document.getElementById(%q);
  if (!anchor) return JSON.stringify({found: false});
  // Walk up to the nearest Documentation-* section container.
  var section = anchor.closest(
    '.Documentation-function, .Documentation-type, .Documentation-method, .Documentation-constant'
  );
  if (!section) return JSON.stringify({found: false});
  var sig  = (section.querySelector('pre') || {}).innerText || '';
  var body = Array.from(section.querySelectorAll('p'))
               .map(p => p.innerText.trim()).join('\n\n');
  return JSON.stringify({found: true, sig: sig, body: body});
})()
`, symbol)

	raw, err := evaluate(pw, script)
	if err != nil {
		return "", fmt.Errorf("evaluate: %w", err)
	}
	raw = strings.Trim(raw, `"`)
	raw = strings.ReplaceAll(raw, `\"`, `"`)

	if strings.Contains(raw, `"found":false`) {
		return fmt.Sprintf("Symbol %q not found in %s.", symbol, pkgPath), nil
	}

	get := func(key string) string {
		marker := `"` + key + `":"`
		start := strings.Index(raw, marker)
		if start < 0 {
			return ""
		}
		start += len(marker)
		end := start
		for end < len(raw) {
			if raw[end] == '"' && raw[end-1] != '\\' {
				break
			}
			end++
		}
		val := raw[start:end]
		return strings.ReplaceAll(val, `\n`, "\n")
	}

	sig := get("sig")
	body := get("body")

	var sb strings.Builder
	fmt.Fprintf(&sb, "# %s.%s\n\nSource: %s\n\n", pkgPath, symbol, url)
	if sig != "" {
		fmt.Fprintf(&sb, "```go\n%s\n```\n\n", sig)
	}
	if body != "" {
		sb.WriteString(body)
	}
	return sb.String(), nil
}

// searchGoDocs searches pkg.go.dev and returns a ranked list of packages.
func searchGoDocs(_ context.Context, pw *PlaywrightProxy, query string, limit int) (string, error) {
	url := fmt.Sprintf("%s/search?q=%s&m=package", pkgBase,
		strings.NewReplacer(" ", "+", "&", "%26", "=", "%3D").Replace(query))
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
      name:     link     ? link.innerText.trim()     : '',
      path:     link     ? link.getAttribute('href').replace(/^\//, '') : '',
      synopsis: synopsis ? synopsis.innerText.trim() : '',
    };
  }));
})()
`, limit)

	raw, err := evaluate(pw, script)
	if err != nil {
		return "", fmt.Errorf("evaluate: %w", err)
	}
	raw = strings.Trim(raw, `"`)
	raw = strings.ReplaceAll(raw, `\"`, `"`)

	// raw is a JSON array of objects – parse it manually to avoid importing encoding/json.
	// Each object looks like: {"name":"...","path":"...","synopsis":"..."}
	var results []string
	entries := strings.Split(raw, `{"name":`)
	for _, entry := range entries[1:] { // skip leading empty before first object
		getName := func(key string) string {
			marker := `"` + key + `":"`
			start := strings.Index(entry, marker)
			if start < 0 {
				return ""
			}
			start += len(marker)
			end := start
			for end < len(entry) {
				if entry[end] == '"' && entry[end-1] != '\\' {
					break
				}
				end++
			}
			return entry[start:end]
		}
		name := getName("name")
		path := getName("path")
		synopsis := getName("synopsis")
		if name == "" && path == "" {
			continue
		}
		line := fmt.Sprintf("- **%s** (`%s`)", name, path)
		if synopsis != "" {
			line += "\n  " + synopsis
		}
		results = append(results, line)
	}

	if len(results) == 0 {
		return fmt.Sprintf("No results found for %q.", query), nil
	}
	return fmt.Sprintf("Search results for %q:\n\n%s", query, strings.Join(results, "\n\n")), nil
}
