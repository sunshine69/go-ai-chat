// rustdoc.go
//
// Registers three MCP tools that fetch Rust documentation from docs.rs and
// crates.io, following the same file-per-source convention as godoc.go.
//
//   fetch_rustdoc_crate – crate overview + item listing (structs, traits, enums, functions, macros)
//   fetch_rustdoc_item  – a single item's page (struct/trait/enum/fn/macro/type)
//   search_crates       – crates.io search (JSON API — no HTML scraping needed)
//
// Unlike godoc.go, this does NOT go through the Playwright proxy: docs.rs
// serves fully static, server-rendered rustdoc HTML, so a plain HTTP GET is
// enough — there's nothing client-side-rendered to wait on. This sidesteps
// the whole class of bugs we just fought through in godoc.go/playwright.go
// (tool schema drift, markdown-envelope unwrapping, etc.) — there's no MCP
// browser tool in the loop here at all.
//
// New dependency: github.com/PuerkitoBio/goquery
//   go get github.com/PuerkitoBio/goquery
//
// NOTE ON SELECTORS: the CSS selectors below (#main-content, .item-table,
// .item-name, .desc, pre.item-decl, .docblock) are based on rustdoc's
// 2024/2025-era HTML output, which has historically been stable but is
// NOT verified against a live page from this environment — I don't have
// network access to docs.rs here. If fetch_rustdoc_crate returns "page
// loaded but no overview or item groups matched known selectors", that
// error is designed to tell you exactly that: open the URL in a browser,
// view source, and adjust the selectors below to match. Same story as
// pkg.go.dev's selectors in godoc.go — expect one iteration.

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/PuerkitoBio/goquery"
	mcp "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

const (
	docsRSBase   = "https://docs.rs"
	cratesIOBase = "https://crates.io/api/v1"

	// rustHTTPTimeout bounds a single fetch; docs.rs pages can be large.
	rustHTTPTimeout = 15 * time.Second

	// cratesIOUserAgent is required by crates.io's API usage policy: it
	// must identify the application, ideally with contact info, or requests
	// can be rejected/rate-limited harder. Update the contact detail below
	// before relying on this in anything long-running.
	cratesIOUserAgent = "mcp-rustdoc-tool/1.0 (contact: sunshine69 on GitHub)"
)

var rustHTTPClient = &http.Client{Timeout: rustHTTPTimeout}

// ── registration ──────────────────────────────────────────────────────────────

// registerRustDocTools adds the three rustdoc/crates.io tools to the MCP
// server. Call it alongside registerGoDocTools / registerPlaywrightTools
// wherever those are wired up — it has no dependency on PlaywrightProxy.
func registerRustDocTools(s *server.MCPServer) {

	// ── fetch_rustdoc_crate ──────────────────────────────────────────────────

	s.AddTool(
		mcp.NewTool("fetch_rustdoc_crate",
			mcp.WithDescription(
				"Fetch Rust crate documentation from docs.rs. "+
					"Returns the crate-level (or module-level) overview plus a listing of "+
					"structs, enums, traits, functions, macros, and type definitions. "+
					"If the result is large it is saved to a temp file and the path is returned "+
					"so you can use text_toc / text_section / text_grep to extract what you need.",
			),
			mcp.WithString("crate_name",
				mcp.Required(),
				mcp.Description(`Crate name as published on crates.io, e.g. "tokio" or "serde_json"`),
			),
			mcp.WithString("version",
				mcp.Description(`Crate version, e.g. "1.38.0". Omit or use "latest" for the newest release.`),
			),
			mcp.WithString("module_path",
				mcp.Description(`Sub-module path within the crate, e.g. "sync/mpsc". Omit for the crate root.`),
			),
		),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			crateName := req.GetString("crate_name", "")
			version := req.GetString("version", "latest")
			modulePath := req.GetString("module_path", "")
			if crateName == "" {
				return toolErr("crate_name is required"), nil
			}
			result, err := fetchRustDocCrate(ctx, crateName, version, modulePath)
			if err != nil {
				return toolErr("%v", err), nil
			}
			hint := fmt.Sprintf("use text_section with heading names like \"Structs\" or \"Traits\" to drill into %s", crateName)
			return maybeOffload(result, hint)
		},
	)

	// ── fetch_rustdoc_item ───────────────────────────────────────────────────

	s.AddTool(
		mcp.NewTool("fetch_rustdoc_item",
			mcp.WithDescription(
				"Fetch documentation for a specific Rust item (struct, enum, trait, fn, "+
					"macro, type alias, or constant) from docs.rs. "+
					"If the result is large it is saved to a temp file and the path is returned.",
			),
			mcp.WithString("crate_name",
				mcp.Required(),
				mcp.Description(`Crate name as published on crates.io, e.g. "tokio"`),
			),
			mcp.WithString("item_type",
				mcp.Required(),
				mcp.Description(`One of: "struct", "enum", "trait", "fn", "macro", "type", "constant"`),
			),
			mcp.WithString("item_name",
				mcp.Required(),
				mcp.Description(`Item name as it appears in the URL, e.g. "Runtime" for struct.Runtime.html`),
			),
			mcp.WithString("version",
				mcp.Description(`Crate version, e.g. "1.38.0". Omit or use "latest" for the newest release.`),
			),
			mcp.WithString("module_path",
				mcp.Description(`Sub-module path the item lives in, e.g. "sync/mpsc". Omit for the crate root.`),
			),
		),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			crateName := req.GetString("crate_name", "")
			itemType := req.GetString("item_type", "")
			itemName := req.GetString("item_name", "")
			version := req.GetString("version", "latest")
			modulePath := req.GetString("module_path", "")
			if crateName == "" || itemType == "" || itemName == "" {
				return toolErr("crate_name, item_type, and item_name are required"), nil
			}
			result, err := fetchRustDocItem(ctx, crateName, itemType, itemName, version, modulePath)
			if err != nil {
				return toolErr("%v", err), nil
			}
			return maybeOffload(result, "")
		},
	)

	// ── search_crates ────────────────────────────────────────────────────────

	s.AddTool(
		mcp.NewTool("search_crates",
			mcp.WithDescription(
				"Search crates.io for Rust crates matching a query. "+
					"Uses the crates.io JSON API directly — no HTML scraping, so this one "+
					"isn't subject to markup drift the way the docs.rs tools are. "+
					"Returns crate names, latest versions, download counts, and descriptions.",
			),
			mcp.WithString("query",
				mcp.Required(),
				mcp.Description(`Search keywords, e.g. "async http client"`),
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
			result, err := searchCrates(ctx, query, limit)
			if err != nil {
				return toolErr("%v", err), nil
			}
			hint := fmt.Sprintf("use text_grep with pattern %q to filter results by keyword", query)
			return maybeOffload(result, hint)
		},
	)
}

// ── URL helpers ───────────────────────────────────────────────────────────────

// rustModulePath converts a published crate name into the identifier docs.rs
// uses in the URL path after the version segment — hyphens become
// underscores, since Rust module/identifier names can't contain hyphens.
// e.g. crate "tokio-util" is served under module path "tokio_util".
func rustModulePath(crateName string) string {
	return strings.ReplaceAll(crateName, "-", "_")
}

func crateRootURL(crateName, version, modulePath string) string {
	mod := rustModulePath(crateName)
	pageURL := fmt.Sprintf("%s/%s/%s/%s/", docsRSBase, crateName, version, mod)
	if modulePath != "" {
		pageURL += strings.Trim(modulePath, "/") + "/"
	}
	return pageURL
}

func itemPageURL(crateName, itemType, itemName, version, modulePath string) string {
	root := crateRootURL(crateName, version, modulePath)
	return fmt.Sprintf("%s%s.%s.html", root, itemType, itemName)
}

// ── fetch helper ──────────────────────────────────────────────────────────────

func fetchHTML(ctx context.Context, pageURL string) (*goquery.Document, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, pageURL, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("User-Agent", cratesIOUserAgent)

	resp, err := rustHTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch %s: %w", pageURL, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("not found (404): %s (check crate_name/version/item spelling)", pageURL)
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 500))
		return nil, fmt.Errorf("unexpected status %d fetching %s: %s", resp.StatusCode, pageURL, string(body))
	}

	doc, err := goquery.NewDocumentFromReader(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("parse HTML from %s: %w", pageURL, err)
	}
	return doc, nil
}

// ── fetchRustDocCrate ─────────────────────────────────────────────────────────

func fetchRustDocCrate(ctx context.Context, crateName, version, modulePath string) (string, error) {
	pageURL := crateRootURL(crateName, version, modulePath)
	doc, err := fetchHTML(ctx, pageURL)
	if err != nil {
		return "", err
	}

	main := doc.Find("#main-content")
	if main.Length() == 0 {
		// docs.rs may be mid-build ("we're building it now, refresh in a
		// minute") or the page structure has changed. Return raw body text
		// so the caller sees something informative instead of a blank
		// result or a confusing downstream JSON error.
		bodyText := strings.TrimSpace(doc.Find("body").Text())
		if bodyText == "" {
			return "", fmt.Errorf("no #main-content found and body is empty — page may not have loaded: %s", pageURL)
		}
		return fmt.Sprintf(
			"# %s\n\nSource: %s\n\n(unrecognized page structure — raw text follows, may indicate the crate is still building or docs.rs markup changed)\n\n%s",
			crateName, pageURL, truncateText(bodyText, 4000),
		), nil
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "# %s\n\nSource: %s\n\n", crateName, pageURL)

	// Crate/module-level overview: the first docblock directly under
	// #main-content, before any item-group headings.
	overview := strings.TrimSpace(main.Find("> .docblock").First().Text())
	if overview != "" {
		sb.WriteString(overview)
		sb.WriteString("\n\n")
	}

	// Item groups: rustdoc renders each kind (Structs, Enums, Traits, ...)
	// as an <h2> heading followed by a list of entries. Walk each h2 and
	// grab list items up to the next h2.
	foundAnyGroup := false
	main.Find("h2").Each(func(_ int, h2 *goquery.Selection) {
		heading := strings.TrimSpace(h2.Find("a").Remove().End().Text())
		if heading == "" {
			heading = strings.TrimSpace(h2.Text())
		}
		items := h2.NextUntil("h2").Find("li")
		if items.Length() == 0 {
			return
		}
		foundAnyGroup = true
		fmt.Fprintf(&sb, "## %s\n\n", heading)
		items.Each(func(_ int, li *goquery.Selection) {
			name := strings.TrimSpace(li.Find(".item-name").First().Text())
			desc := strings.TrimSpace(li.Find(".desc").First().Text())
			if name == "" {
				name = strings.TrimSpace(li.Text())
			}
			if desc != "" {
				fmt.Fprintf(&sb, "- **%s** — %s\n", name, desc)
			} else {
				fmt.Fprintf(&sb, "- **%s**\n", name)
			}
		})
		sb.WriteString("\n")
	})

	if !foundAnyGroup && overview == "" {
		return "", fmt.Errorf(
			"page loaded but no overview or item groups matched known selectors — rustdoc markup may have changed; inspect %s directly and adjust the selectors in rustdoc.go",
			pageURL,
		)
	}

	return strings.TrimSpace(sb.String()), nil
}

// ── fetchRustDocItem ──────────────────────────────────────────────────────────

func fetchRustDocItem(ctx context.Context, crateName, itemType, itemName, version, modulePath string) (string, error) {
	pageURL := itemPageURL(crateName, itemType, itemName, version, modulePath)
	doc, err := fetchHTML(ctx, pageURL)
	if err != nil {
		return "", err
	}

	main := doc.Find("#main-content")
	if main.Length() == 0 {
		return "", fmt.Errorf("no #main-content found at %s — rustdoc markup may have changed", pageURL)
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "# %s::%s\n\nSource: %s\n\n", crateName, itemName, pageURL)

	// Signature: rustdoc renders the item's declaration in a
	// <pre class="rust item-decl">...</pre> block near the top of the page.
	sig := strings.TrimSpace(main.Find("pre.item-decl").First().Text())
	if sig != "" {
		fmt.Fprintf(&sb, "```rust\n%s\n```\n\n", sig)
	}

	// Docblocks: item-level docs, plus any per-method docblocks for
	// structs/traits with associated items on the same page.
	var docParts []string
	main.Find(".docblock").Each(func(_ int, s *goquery.Selection) {
		text := strings.TrimSpace(s.Text())
		if text != "" {
			docParts = append(docParts, text)
		}
	})
	if len(docParts) > 0 {
		sb.WriteString(strings.Join(docParts, "\n\n"))
	} else if sig == "" {
		return "", fmt.Errorf(
			"page loaded but no signature or docblocks matched known selectors — rustdoc markup may have changed; inspect %s directly and adjust the selectors in rustdoc.go",
			pageURL,
		)
	}

	return strings.TrimSpace(sb.String()), nil
}

// ── searchCrates ──────────────────────────────────────────────────────────────

type crateSearchResponse struct {
	Crates []struct {
		Name        string `json:"name"`
		MaxVersion  string `json:"max_version"`
		Description string `json:"description"`
		Downloads   int64  `json:"downloads"`
	} `json:"crates"`
}

func searchCrates(ctx context.Context, query string, limit int) (string, error) {
	apiURL := fmt.Sprintf("%s/crates?q=%s&per_page=%d", cratesIOBase, url.QueryEscape(query), limit)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil)
	if err != nil {
		return "", fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("User-Agent", cratesIOUserAgent)
	req.Header.Set("Accept", "application/json")

	resp, err := rustHTTPClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("fetch crates.io search: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 500))
		return "", fmt.Errorf("unexpected status %d from crates.io: %s", resp.StatusCode, string(body))
	}

	var parsed crateSearchResponse
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return "", fmt.Errorf("parse crates.io response: %w", err)
	}

	if len(parsed.Crates) == 0 {
		return fmt.Sprintf("No results found for %q.", query), nil
	}

	var lines []string
	for _, c := range parsed.Crates {
		line := fmt.Sprintf("- **%s** (`%s`, %s downloads)", c.Name, c.MaxVersion, formatWithCommas(c.Downloads))
		if c.Description != "" {
			line += "\n  " + c.Description
		}
		lines = append(lines, line)
	}

	return fmt.Sprintf("# crates.io search results for %q\n\n%s", query, strings.Join(lines, "\n\n")), nil
}

// ── small helpers ─────────────────────────────────────────────────────────────

func truncateText(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "\n...(truncated)"
}

func formatWithCommas(n int64) string {
	s := strconv.FormatInt(n, 10)
	var out []byte
	for i, c := range []byte(s) {
		if i != 0 && (len(s)-i)%3 == 0 {
			out = append(out, ',')
		}
		out = append(out, c)
	}
	return string(out)
}
