package main

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// TextToolManager provides Unix-style text extraction tools for non-Unix hosts (e.g. Windows).
// Primary use-case: an AI agent that has saved a large file (e.g. fetched markdown doc) to disk
// and needs to efficiently extract relevant sections without reading the whole file into context.
//
// Recommended AI workflow for a large markdown file:
//  1. text_wc        – check line/byte count to understand size
//  2. text_toc       – list all headings with line numbers to map the document
//  3. text_section   – extract a full section by heading name
//  4. text_grep      – search for a term when the section name is unknown
//  5. text_lines     – pull an exact line range after grep/toc reveals the position
//  6. text_head/tail – quick orientation at top or bottom of file
//  7. text_cat       – last resort; capped at max_lines (default 200)
type TextToolManager struct{}

// ── internal helpers ─────────────────────────────────────────────────────────

func readLines(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("cannot open file %q: %w", path, err)
	}
	defer f.Close()

	var lines []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1024*1024), 10*1024*1024) // handles up to 10 MB/line (minified files)
	for sc.Scan() {
		lines = append(lines, sc.Text())
	}
	return lines, sc.Err()
}

func textResult(s string) *mcp.CallToolResult {
	return &mcp.CallToolResult{
		Content: []mcp.Content{
			mcp.TextContent{Type: "text", Text: s},
		},
	}
}

func errResult(err error) *mcp.CallToolResult {
	return textResult("ERROR: " + err.Error())
}

func clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// formatBlock renders a slice of lines with their 1-based file line numbers.
// startLine is the 1-based index of lines[0] in the original file.
func formatBlock(lines []string, startLine int, showNums bool) string {
	if !showNums {
		return strings.Join(lines, "\n")
	}
	var sb strings.Builder
	for i, l := range lines {
		fmt.Fprintf(&sb, "%d\t%s\n", startLine+i, l)
	}
	return sb.String()
}

// ── text_grep ────────────────────────────────────────────────────────────────

// handleGrep searches for lines matching a Go regex.
// Line numbers of match blocks are ALWAYS included in the output header so the
// AI can issue a follow-up text_lines call without re-running grep.
func (t *TextToolManager) handleGrep(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	file := req.GetString("file", "")
	pattern := req.GetString("pattern", "")
	before := req.GetInt("before", 0)
	after := req.GetInt("after", 0)
	invert := req.GetBool("invert", false)
	onlyMatch := req.GetBool("only_match", false)
	countOnly := req.GetBool("count", false)
	maxMatches := req.GetInt("max_matches", 0)

	if file == "" || pattern == "" {
		return errResult(fmt.Errorf("file and pattern are required")), nil
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return errResult(fmt.Errorf("invalid regex %q: %w", pattern, err)), nil
	}

	lines, err := readLines(file)
	if err != nil {
		return errResult(err), nil
	}

	// Identify matching line indices.
	matched := make([]bool, len(lines))
	matchCount := 0
	for i, l := range lines {
		m := re.MatchString(l)
		if invert {
			m = !m
		}
		if m {
			matchCount++
			if maxMatches == 0 || matchCount <= maxMatches {
				matched[i] = true
			}
		}
	}

	if countOnly {
		return textResult(strconv.Itoa(matchCount)), nil
	}

	// Mark lines to print (match + context window).
	printed := make([]bool, len(lines))
	for i, m := range matched {
		if !m {
			continue
		}
		for j := clamp(i-before, 0, len(lines)-1); j <= clamp(i+after, 0, len(lines)-1); j++ {
			printed[j] = true
		}
	}

	// Render output.
	// Each contiguous printed block is prefixed with "# lines N-M" so the AI
	// always knows the absolute position, even without requesting line numbers.
	var sb strings.Builder
	inBlock := false
	blockStart := 0

	flush := func(blockLines []string, start int) {
		if len(blockLines) == 0 {
			return
		}
		end := start + len(blockLines) - 1
		if start == end {
			fmt.Fprintf(&sb, "# line %d\n", start)
		} else {
			fmt.Fprintf(&sb, "# lines %d-%d\n", start, end)
		}
		for i, l := range blockLines {
			lineNo := start + i
			isMatch := lineNo >= 1 && lineNo <= len(lines) && matched[lineNo-1]
			if isMatch && onlyMatch && !invert {
				all := re.FindAllString(l, -1)
				fmt.Fprintf(&sb, "%d\t%s\n", lineNo, strings.Join(all, ", "))
			} else {
				fmt.Fprintf(&sb, "%d\t%s\n", lineNo, l)
			}
		}
	}

	var blockLines []string
	for i, l := range lines {
		lineNo := i + 1
		if printed[i] {
			if !inBlock {
				inBlock = true
				blockStart = lineNo
				blockLines = blockLines[:0]
			}
			blockLines = append(blockLines, l)
		} else {
			if inBlock {
				flush(blockLines, blockStart)
				sb.WriteString("---\n")
				blockLines = nil
			}
			inBlock = false
		}
	}
	if inBlock {
		flush(blockLines, blockStart)
	}

	out := strings.TrimSuffix(sb.String(), "---\n")
	if out == "" {
		out = "(no matches)"
	}
	return textResult(out), nil
}

// ── text_head ────────────────────────────────────────────────────────────────

// handleHead returns the first n lines (default 10).
// Negative n: all lines except the last |n| (GNU head -n -N behaviour).
func (t *TextToolManager) handleHead(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	file := req.GetString("file", "")
	n := req.GetInt("lines", 10)
	showNums := req.GetBool("line_number", false)

	if file == "" {
		return errResult(fmt.Errorf("file is required")), nil
	}
	lines, err := readLines(file)
	if err != nil {
		return errResult(err), nil
	}

	end := n
	if n < 0 {
		end = len(lines) + n
	}
	end = clamp(end, 0, len(lines))

	return textResult(formatBlock(lines[:end], 1, showNums)), nil
}

// ── text_tail ────────────────────────────────────────────────────────────────

// handleTail returns the last n lines (default 10).
// Negative n: skip the first |n| lines.
func (t *TextToolManager) handleTail(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	file := req.GetString("file", "")
	n := req.GetInt("lines", 10)
	showNums := req.GetBool("line_number", false)

	if file == "" {
		return errResult(fmt.Errorf("file is required")), nil
	}
	lines, err := readLines(file)
	if err != nil {
		return errResult(err), nil
	}

	start := len(lines) - n
	if n < 0 {
		start = -n
	}
	start = clamp(start, 0, len(lines))

	return textResult(formatBlock(lines[start:], start+1, showNums)), nil
}

// ── text_lines ───────────────────────────────────────────────────────────────

// handleLines extracts lines [from, to] (1-based, inclusive).
// This is the primary follow-up tool after text_grep returns a line number.
func (t *TextToolManager) handleLines(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	file := req.GetString("file", "")
	from := req.GetInt("from", 1)
	to := req.GetInt("to", -1) // -1 = EOF
	showNums := req.GetBool("line_number", false)

	if file == "" {
		return errResult(fmt.Errorf("file is required")), nil
	}
	lines, err := readLines(file)
	if err != nil {
		return errResult(err), nil
	}

	from = clamp(from, 1, len(lines))
	if to < 0 || to > len(lines) {
		to = len(lines)
	}
	if from > to {
		return textResult("(empty range)"), nil
	}

	return textResult(formatBlock(lines[from-1:to], from, showNums)), nil
}

// ── text_section ─────────────────────────────────────────────────────────────

// handleSection extracts a complete markdown section by heading text.
// It finds the first heading containing the search string (case-insensitive),
// then returns all lines up to (but not including) the next heading at the same
// or higher level.
func (t *TextToolManager) handleSection(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	file := req.GetString("file", "")
	heading := req.GetString("heading", "")
	includeSubsections := req.GetBool("include_subsections", true)
	maxLines := req.GetInt("max_lines", 300)

	if file == "" || heading == "" {
		return errResult(fmt.Errorf("file and heading are required")), nil
	}

	lines, err := readLines(file)
	if err != nil {
		return errResult(err), nil
	}

	headingLevel := func(line string) int {
		for i, ch := range line {
			if ch != '#' {
				if i > 0 && len(line) > i && line[i] == ' ' {
					return i
				}
				return 0
			}
		}
		return 0
	}

	searchLower := strings.ToLower(heading)

	// Find the target heading line.
	targetIdx := -1
	targetLevel := 0
	for i, l := range lines {
		lvl := headingLevel(l)
		if lvl == 0 {
			continue
		}
		text := strings.TrimSpace(strings.TrimLeft(l, "#"))
		if strings.Contains(strings.ToLower(text), searchLower) {
			targetIdx = i
			targetLevel = lvl
			break
		}
	}

	if targetIdx < 0 {
		return textResult(fmt.Sprintf("(no heading containing %q found)", heading)), nil
	}

	// Collect lines until the next heading at the same or higher level.
	end := len(lines)
	for i := targetIdx + 1; i < len(lines); i++ {
		lvl := headingLevel(lines[i])
		if lvl == 0 {
			continue
		}
		if lvl <= targetLevel {
			end = i
			break
		}
		if !includeSubsections && lvl > targetLevel {
			end = i
			break
		}
	}

	section := lines[targetIdx:end]
	truncated := false
	if maxLines > 0 && len(section) > maxLines {
		section = section[:maxLines]
		truncated = true
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "# section found at line %d\n", targetIdx+1)
	for i, l := range section {
		fmt.Fprintf(&sb, "%d\t%s\n", targetIdx+1+i, l)
	}
	if truncated {
		fmt.Fprintf(&sb, "\n[truncated after %d lines — use text_lines to read more starting at line %d]\n",
			maxLines, targetIdx+1+maxLines)
	}
	return textResult(sb.String()), nil
}

// ── text_toc ─────────────────────────────────────────────────────────────────

// handleTOC scans the file and returns every markdown heading with its level,
// 1-based line number, and indented text — giving the AI a complete map of the
// document in a single cheap call before deciding which section to fetch.
func (t *TextToolManager) handleTOC(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	file := req.GetString("file", "")
	maxDepth := req.GetInt("max_depth", 0)

	if file == "" {
		return errResult(fmt.Errorf("file is required")), nil
	}

	lines, err := readLines(file)
	if err != nil {
		return errResult(err), nil
	}

	headingLevel := func(line string) int {
		for i, ch := range line {
			if ch != '#' {
				if i > 0 && len(line) > i && line[i] == ' ' {
					return i
				}
				return 0
			}
		}
		return 0
	}

	var sb strings.Builder
	count := 0
	for i, l := range lines {
		lvl := headingLevel(l)
		if lvl == 0 {
			continue
		}
		if maxDepth > 0 && lvl > maxDepth {
			continue
		}
		text := strings.TrimSpace(strings.TrimLeft(l, "#"))
		indent := strings.Repeat("  ", lvl-1)
		fmt.Fprintf(&sb, "%d\t%s%s %s\n", i+1, indent, strings.Repeat("#", lvl), text)
		count++
	}

	if count == 0 {
		return textResult("(no headings found)"), nil
	}
	return textResult(sb.String()), nil
}

// ── text_wc ──────────────────────────────────────────────────────────────────

// handleWc counts lines, words, and bytes.
// Should be the AI's first call on an unknown file to decide the read strategy.
func (t *TextToolManager) handleWc(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	file := req.GetString("file", "")
	if file == "" {
		return errResult(fmt.Errorf("file is required")), nil
	}
	info, err := os.Stat(file)
	if err != nil {
		return errResult(fmt.Errorf("cannot stat file: %w", err)), nil
	}
	lines, err := readLines(file)
	if err != nil {
		return errResult(err), nil
	}
	words := 0
	for _, l := range lines {
		words += len(strings.Fields(l))
	}
	return textResult(fmt.Sprintf("lines: %d\nwords: %d\nbytes: %d", len(lines), words, info.Size())), nil
}

// ── text_cat ─────────────────────────────────────────────────────────────────

// handleCat dumps the file, hard-capped at max_lines (default 200, 0 = unlimited).
// For large files the AI should prefer text_grep / text_section / text_lines.
func (t *TextToolManager) handleCat(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	file := req.GetString("file", "")
	showNums := req.GetBool("line_number", false)
	maxLines := req.GetInt("max_lines", 200)

	if file == "" {
		return errResult(fmt.Errorf("file is required")), nil
	}
	lines, err := readLines(file)
	if err != nil {
		return errResult(err), nil
	}

	truncated := false
	if maxLines > 0 && len(lines) > maxLines {
		lines = lines[:maxLines]
		truncated = true
	}

	var sb strings.Builder
	sb.WriteString(formatBlock(lines, 1, showNums))
	if truncated {
		fmt.Fprintf(&sb, "\n[truncated at %d lines — use text_grep or text_lines to read further]\n", maxLines)
	}
	return textResult(sb.String()), nil
}

// ── registration ─────────────────────────────────────────────────────────────

func registerTextTools(s *server.MCPServer, tool *TextToolManager) {
	// ── text_grep ──
	s.AddTool(mcp.NewTool("text_grep",
		mcp.WithDescription(
			"Search a file for lines matching a Go regular expression. "+
				"Always returns 1-based line numbers in block headers so the AI can "+
				"issue a follow-up text_lines call without re-running grep. "+
				"Supports context lines (before/after), print-match-only, invert, count, and match cap. "+
				"Designed for non-Unix hosts (Windows) where grep is unavailable.",
		),
		mcp.WithString("file", mcp.Required(), mcp.Description("Path to the file to search.")),
		mcp.WithString("pattern", mcp.Required(), mcp.Description("Go regular expression (https://pkg.go.dev/regexp/syntax).")),
		mcp.WithNumber("before", mcp.Description("Lines of context before each match (-B). Default: 0.")),
		mcp.WithNumber("after", mcp.Description("Lines of context after each match (-A). Default: 0.")),
		mcp.WithBoolean("invert", mcp.Description("Print lines that do NOT match (-v). Default: false.")),
		mcp.WithBoolean("only_match", mcp.Description("Print only the matched sub-string per line (-o). Default: false.")),
		mcp.WithBoolean("count", mcp.Description("Print only the total count of matching lines (-c). Default: false.")),
		mcp.WithNumber("max_matches", mcp.Description("Stop after this many matching lines (0 = unlimited). Useful for large files. Default: 0.")),
	), tool.handleGrep)

	// ── text_head ──
	s.AddTool(mcp.NewTool("text_head",
		mcp.WithDescription("Print the first N lines of a file. Negative N: all but last |N| lines (GNU head -n -N)."),
		mcp.WithString("file", mcp.Required(), mcp.Description("Path to the file.")),
		mcp.WithNumber("lines", mcp.Description("Lines from the top. Default: 10.")),
		mcp.WithBoolean("line_number", mcp.Description("Prefix each line with its 1-based line number. Default: false.")),
	), tool.handleHead)

	// ── text_tail ──
	s.AddTool(mcp.NewTool("text_tail",
		mcp.WithDescription("Print the last N lines of a file. Negative N: skip first |N| lines."),
		mcp.WithString("file", mcp.Required(), mcp.Description("Path to the file.")),
		mcp.WithNumber("lines", mcp.Description("Lines from the bottom. Default: 10.")),
		mcp.WithBoolean("line_number", mcp.Description("Prefix each line with its 1-based line number. Default: false.")),
	), tool.handleTail)

	// ── text_lines ──
	s.AddTool(mcp.NewTool("text_lines",
		mcp.WithDescription(
			"Extract a specific range of lines from a file (1-based, inclusive). "+
				"Primary follow-up tool after text_grep returns a line number. "+
				"Omit 'to' to read from 'from' to end-of-file.",
		),
		mcp.WithString("file", mcp.Required(), mcp.Description("Path to the file.")),
		mcp.WithNumber("from", mcp.Description("First line to include (1-based). Default: 1.")),
		mcp.WithNumber("to", mcp.Description("Last line to include (1-based, inclusive). Omit or -1 for EOF.")),
		mcp.WithBoolean("line_number", mcp.Description("Prefix each line with its 1-based line number. Default: false.")),
	), tool.handleLines)

	// ── text_section ──
	s.AddTool(mcp.NewTool("text_section",
		mcp.WithDescription(
			"Extract a complete markdown section by heading text. "+
				"Finds the first heading containing the search string (case-insensitive) "+
				"and returns all content up to the next heading at the same or higher level. "+
				"The most efficient single call when navigating a large markdown document.",
		),
		mcp.WithString("file", mcp.Required(), mcp.Description("Path to the markdown file.")),
		mcp.WithString("heading", mcp.Required(), mcp.Description("Substring to search for in heading text (case-insensitive). E.g. \"installation\" or \"API reference\".")),
		mcp.WithBoolean("include_subsections", mcp.Description("Include deeper headings inside the section (default: true).")),
		mcp.WithNumber("max_lines", mcp.Description("Cap output at this many lines. Default: 300. 0 = unlimited.")),
	), tool.handleSection)

	// ── text_toc ──
	s.AddTool(mcp.NewTool("text_toc",
		mcp.WithDescription(
			"List all markdown headings in a file as a table of contents, "+
				"each with its 1-based line number and indented hierarchy. "+
				"Call this first to map a document's structure, then use "+
				"text_section with the exact heading name to fetch the content you need.",
		),
		mcp.WithString("file", mcp.Required(), mcp.Description("Path to the markdown file.")),
		mcp.WithNumber("max_depth", mcp.Description("Only include headings up to this level (1=top-level only, 2=h1+h2, etc). Default: 0 = all levels.")),
	), tool.handleTOC)

	// ── text_wc ──
	s.AddTool(mcp.NewTool("text_wc",
		mcp.WithDescription(
			"Count lines, words, and bytes in a file. "+
				"Call this first on an unknown file to decide the appropriate read strategy "+
				"(grep vs section vs cat).",
		),
		mcp.WithString("file", mcp.Required(), mcp.Description("Path to the file.")),
	), tool.handleWc)

	// ── text_cat ──
	s.AddTool(mcp.NewTool("text_cat",
		mcp.WithDescription(
			"Print file contents, hard-capped at max_lines (default 200) to protect context window. "+
				"For large files prefer text_grep, text_section, or text_lines instead.",
		),
		mcp.WithString("file", mcp.Required(), mcp.Description("Path to the file.")),
		mcp.WithBoolean("line_number", mcp.Description("Prefix each line with its 1-based line number. Default: false.")),
		mcp.WithNumber("max_lines", mcp.Description("Hard cap on lines returned (0 = unlimited). Default: 200.")),
	), tool.handleCat)
}
