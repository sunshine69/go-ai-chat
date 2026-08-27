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
//  7. text_sed       – edit the file in place (substitute/delete lines) once you know what to change
//  8. text_cat       – last resort; capped at max_lines (default 200)
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

// unescapeReplacement converts a handful of backslash escape sequences in a
// sed replacement string into their literal characters: \n -> newline,
// \t -> tab, \r -> carriage return, \\ -> backslash.
//
// This exists because Go's regexp.ReplaceAllString/ExpandString only treats
// "$" specially in the replacement — unlike the pattern side (parsed by RE2,
// which already understands \n/\t natively), a literal two-character "\n" in
// the replacement is passed through unchanged and would otherwise show up in
// the file as a literal backslash-n instead of a real line break. Any other
// backslash sequence (e.g. "\d") is left untouched so it doesn't silently eat
// characters the caller didn't mean to escape.
func unescapeReplacement(s string) string {
	if !strings.Contains(s, "\\") {
		return s
	}
	var sb strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) {
			switch s[i+1] {
			case 'n':
				sb.WriteByte('\n')
				i++
				continue
			case 't':
				sb.WriteByte('\t')
				i++
				continue
			case 'r':
				sb.WriteByte('\r')
				i++
				continue
			case '\\':
				sb.WriteByte('\\')
				i++
				continue
			}
		}
		sb.WriteByte(s[i])
	}
	return sb.String()
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

// ── text_sed ─────────────────────────────────────────────────────────────────
//
// handleSed is a deliberately small subset of sed, aimed at being unambiguous
// for an AI model to generate rather than being feature-complete. It edits the
// file IN PLACE (like `sed -i`) — pass dry_run=true to preview the effect of a
// command without writing anything to disk.
//
// Pattern syntax is Go's RE2 (regexp.Compile), the same as text_grep's
// "pattern" argument — NOT POSIX BRE/ERE, NOT PCRE. No backreferences in the
// pattern, no lookahead/lookbehind. Capture groups are usable in the
// replacement string as $1 / ${1} / $name via regexp.ExpandString.
//
// Delimiter is always "|" (no custom delimiter support, to keep the grammar
// unambiguous). Because "|" is the delimiter, patterns/replacements must not
// contain a literal, unescaped "|" — e.g. regex alternation via `a|b` is not
// supported; use a character class or bracket expression instead where possible.
//
// Supported forms:
//
//  1. s|ptn|replace|g          substitute over the whole file, all matches per line
//     s|ptn|replace            substitute over the whole file, first match per line only
//  2. |ptn|d                   delete every line matching ptn
//     |ptn|!d                  delete every line NOT matching ptn (i.e. keep only matches)
//  3. 5d                       delete line 5
//     5!d                      delete every line EXCEPT line 5
//  4. 1,5d                     delete lines 1 through 5 (inclusive)
//     1,5!d                    delete every line EXCEPT lines 1 through 5
//  5. 2,5s|old|new|g           substitute, but only within lines 2 through 5
//     5,$s|old|new             substitute from line 5 to end of file ("$" = last line),
//     first match per line only
//  6. 5c|text                  change (replace) line 5 with "text"
//     5!c|text                 change every line EXCEPT line 5 to "text" (one copy per line)
//  7. 1,5c|text                change lines 1-5, collapsing the whole range into ONE line: "text"
//     1,5!c|text                change every line OUTSIDE 1-5 to "text" (one copy per line)
//  8. |ptn|c|text              change every line matching ptn to "text" (one-for-one)
//     |ptn|!c|text             change every line NOT matching ptn to "text" (one-for-one)
//
// The tool never guesses: any command that doesn't cleanly match one of the
// forms above is rejected with an error listing the supported forms, rather
// than silently doing something unexpected to the file.
func (t *TextToolManager) handleSed(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	file := req.GetString("file", "")
	command := req.GetString("command", "")
	dryRun := req.GetBool("dry_run", false)

	if file == "" || command == "" {
		return errResult(fmt.Errorf("file and command are required")), nil
	}

	lines, err := readLines(file)
	if err != nil {
		return errResult(err), nil
	}
	n := len(lines)

	if n == 0 {
		return textResult(fmt.Sprintf("file %q is empty — nothing to do", file)), nil
	}

	const usage = "unrecognized sed command %q — supported forms: " +
		"s|ptn|repl|g, s|ptn|repl, N,Ms|ptn|repl|g, |ptn|d, |ptn|!d, Nd, N!d, N,Md, N,M!d, " +
		"Nc|text, N!c|text, N,Mc|text, N,M!c|text, |ptn|c|text, |ptn|!c|text"

	resolveAddr := func(s string) (int, error) {
		if s == "$" {
			return n, nil
		}
		v, err := strconv.Atoi(s)
		if err != nil {
			return 0, fmt.Errorf("invalid line address %q", s)
		}
		return v, nil
	}

	parseRange := func(body string) (start, end int, err error) {
		if strings.Contains(body, ",") {
			bits := strings.SplitN(body, ",", 2)
			start, err = resolveAddr(bits[0])
			if err != nil {
				return 0, 0, err
			}
			end, err = resolveAddr(bits[1])
			if err != nil {
				return 0, 0, err
			}
		} else {
			start, err = resolveAddr(body)
			if err != nil {
				return 0, 0, err
			}
			end = start
		}
		if start < 1 || end > n || start > end {
			return 0, 0, fmt.Errorf("line range %d,%d out of bounds (file has %d lines)", start, end, n)
		}
		return start, end, nil
	}

	var outLines []string
	var changed int
	var action string

	parts := strings.Split(command, "|")

	switch {
	// ── |ptn|d  /  |ptn|!d ── pattern-based delete, whole file, no address ──
	case len(parts) == 3 && parts[0] == "" && (parts[2] == "d" || parts[2] == "!d"):
		pattern := parts[1]
		invert := parts[2] == "!d"

		re, err := regexp.Compile(pattern)
		if err != nil {
			return errResult(fmt.Errorf("invalid regex %q: %w", pattern, err)), nil
		}
		outLines = make([]string, 0, n)
		for _, l := range lines {
			isMatch := re.MatchString(l)
			del := isMatch
			if invert {
				del = !isMatch
			}
			if del {
				changed++
				continue
			}
			outLines = append(outLines, l)
		}
		if invert {
			action = fmt.Sprintf("deleted %d line(s) NOT matching /%s/", changed, pattern)
		} else {
			action = fmt.Sprintf("deleted %d line(s) matching /%s/", changed, pattern)
		}

	// ── |ptn|c|text  /  |ptn|!c|text ── pattern-based change, one-for-one ──
	case len(parts) == 4 && parts[0] == "" && (parts[2] == "c" || parts[2] == "!c"):
		pattern := parts[1]
		invert := parts[2] == "!c"
		text := unescapeReplacement(parts[3])

		re, err := regexp.Compile(pattern)
		if err != nil {
			return errResult(fmt.Errorf("invalid regex %q: %w", pattern, err)), nil
		}
		outLines = make([]string, 0, n)
		for _, l := range lines {
			isMatch := re.MatchString(l)
			repl := isMatch
			if invert {
				repl = !isMatch
			}
			if repl {
				changed++
				outLines = append(outLines, text)
				continue
			}
			outLines = append(outLines, l)
		}
		if invert {
			action = fmt.Sprintf("changed %d line(s) NOT matching /%s/ to %q", changed, pattern, text)
		} else {
			action = fmt.Sprintf("changed %d line(s) matching /%s/ to %q", changed, pattern, text)
		}

	// ── [addr]s|ptn|repl|flags  or  [addr]s|ptn|repl ── substitute ──
	case (len(parts) == 3 || len(parts) == 4) && strings.HasSuffix(parts[0], "s"):
		addrPart := strings.TrimSuffix(parts[0], "s")
		start, end := 1, n
		if addrPart != "" {
			start, end, err = parseRange(addrPart)
			if err != nil {
				return errResult(err), nil
			}
		}

		pattern, replacement := parts[1], parts[2]
		replacement = unescapeReplacement(replacement)
		flags := ""
		if len(parts) == 4 {
			flags = parts[3]
		}
		global := strings.Contains(flags, "g")

		re, err := regexp.Compile(pattern)
		if err != nil {
			return errResult(fmt.Errorf("invalid regex %q: %w", pattern, err)), nil
		}

		outLines = make([]string, n)
		copy(outLines, lines)
		for i := start - 1; i <= end-1; i++ {
			orig := outLines[i]
			var replaced string
			if global {
				replaced = re.ReplaceAllString(orig, replacement)
			} else {
				loc := re.FindStringSubmatchIndex(orig)
				if loc == nil {
					replaced = orig
				} else {
					var buf []byte
					buf = re.ExpandString(buf, replacement, orig, loc)
					replaced = orig[:loc[0]] + string(buf) + orig[loc[1]:]
				}
			}
			if replaced != orig {
				changed++
				outLines[i] = replaced
			}
		}
		rangeDesc := fmt.Sprintf("%d", start)
		if start != end {
			rangeDesc = fmt.Sprintf("%d,%d", start, end)
		}
		if addrPart == "" {
			rangeDesc = "whole file"
		}
		action = fmt.Sprintf("substituted on %d line(s) (%s, /%s/ -> /%s/, flags=%q)",
			changed, rangeDesc, pattern, replacement, flags)

	// ── Nd / N!d / N,Md / N,M!d ── numeric line delete, no pattern ──
	case len(parts) == 1:
		body := parts[0]
		invert := false
		switch {
		case strings.HasSuffix(body, "!d"):
			invert = true
			body = strings.TrimSuffix(body, "!d")
		case strings.HasSuffix(body, "d"):
			body = strings.TrimSuffix(body, "d")
		default:
			return errResult(fmt.Errorf(usage, command)), nil
		}
		if body == "" {
			return errResult(fmt.Errorf(usage, command)), nil
		}
		start, end, err := parseRange(body)
		if err != nil {
			return errResult(err), nil
		}
		outLines = make([]string, 0, n)
		for i, l := range lines {
			lineNo := i + 1
			inRange := lineNo >= start && lineNo <= end
			del := inRange
			if invert {
				del = !inRange
			}
			if del {
				changed++
				continue
			}
			outLines = append(outLines, l)
		}
		rangeDesc := fmt.Sprintf("%d", start)
		if start != end {
			rangeDesc = fmt.Sprintf("%d,%d", start, end)
		}
		if invert {
			action = fmt.Sprintf("deleted %d line(s) outside range %s", changed, rangeDesc)
		} else {
			action = fmt.Sprintf("deleted %d line(s) in range %s", changed, rangeDesc)
		}

	// ── Nc|text / N!c|text / N,Mc|text / N,M!c|text ── numeric line change ──
	// A plain (non-inverted) range is a single sed address, so it collapses to
	// ONE instance of text replacing the whole range — matching real sed's
	// `N,Mc\text` behaviour. Inverted forms replace each excluded line
	// individually (those lines aren't a contiguous address), one copy of
	// text per line, same as |ptn|c does.
	case len(parts) == 2 && strings.HasSuffix(parts[0], "c"):
		body := parts[0]
		invert := false
		switch {
		case strings.HasSuffix(body, "!c"):
			invert = true
			body = strings.TrimSuffix(body, "!c")
		default:
			body = strings.TrimSuffix(body, "c")
		}
		if body == "" {
			return errResult(fmt.Errorf(usage, command)), nil
		}
		start, end, err := parseRange(body)
		if err != nil {
			return errResult(err), nil
		}
		text := unescapeReplacement(parts[1])

		outLines = make([]string, 0, n)
		inserted := false
		for i, l := range lines {
			lineNo := i + 1
			inRange := lineNo >= start && lineNo <= end
			repl := inRange
			if invert {
				repl = !inRange
			}
			if repl {
				changed++
				if invert {
					outLines = append(outLines, text) // scattered lines: one copy each
				} else if !inserted {
					outLines = append(outLines, text) // contiguous range: collapse to one copy
					inserted = true
				}
				continue
			}
			outLines = append(outLines, l)
		}
		rangeDesc := fmt.Sprintf("%d", start)
		if start != end {
			rangeDesc = fmt.Sprintf("%d,%d", start, end)
		}
		if invert {
			action = fmt.Sprintf("changed %d line(s) outside range %s to %q (one copy per line)", changed, rangeDesc, text)
		} else {
			action = fmt.Sprintf("changed %d line(s) in range %s to a single line: %q", changed, rangeDesc, text)
		}

	default:
		return errResult(fmt.Errorf(usage, command)), nil
	}

	if dryRun {
		return textResult(fmt.Sprintf("[dry run] would have %s in %q — no changes written (dry_run=true)", action, file)), nil
	}

	if changed == 0 {
		return textResult(fmt.Sprintf("no changes: 0 line(s) affected by %q in %q — file left untouched", command, file)), nil
	}

	// Write in place. This overwrites the original file, so callers that want
	// a safety net should run with dry_run=true first, or keep their own backup
	// (e.g. via version control) — this tool intentionally does not create one,
	// to keep behaviour predictable and match `sed -i` semantics.
	f, err := os.Create(file)
	if err != nil {
		return errResult(fmt.Errorf("cannot write file %q: %w", file, err)), nil
	}
	w := bufio.NewWriter(f)
	for _, l := range outLines {
		if _, err := w.WriteString(l + "\n"); err != nil {
			f.Close()
			return errResult(fmt.Errorf("write error: %w", err)), nil
		}
	}
	if err := w.Flush(); err != nil {
		f.Close()
		return errResult(fmt.Errorf("flush error: %w", err)), nil
	}
	if err := f.Close(); err != nil {
		return errResult(fmt.Errorf("close error: %w", err)), nil
	}

	return textResult(fmt.Sprintf("OK: %s. Wrote %d line(s) to %q.", action, len(outLines), file)), nil
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

	// ── text_sed ──
	s.AddTool(mcp.NewTool("text_sed",
		mcp.WithDescription(
			"Edit a file IN PLACE using a tiny, unambiguous subset of sed. "+
				"PATTERN SYNTAX IS GO RE2 (regexp.Compile / pkg.go.dev/regexp/syntax), the SAME syntax as text_grep's "+
				"'pattern' argument — it is NOT POSIX BRE/ERE and NOT PCRE. "+
				"Practical implications: '+ ? ( ) { } |' are ALREADY special, do not backslash-escape them like POSIX BRE. "+
				"Backreferences inside the pattern (e.g. '(a)\\1') are NOT supported. "+
				"Lookahead/lookbehind are NOT supported. "+
				"Numbered/named capture groups ($1, ${1}, $name) work ONLY in 's' — they are NOT expanded in 'c' text, "+
				"which is inserted literally (after escape processing below). "+
				"The replacement/text string (used by both 's' and 'c') supports these escapes: \\n (newline), "+
				"\\t (tab), \\r (carriage return), \\\\ (literal backslash) — use \\n to split one line into two. "+
				"Any other backslash sequence is left as-is. "+
				"IMPORTANT for the 'c' (change) command: a plain numeric RANGE (e.g. 1,5c|text, no '!') is a single "+
				"address, so ALL matched lines collapse into exactly ONE output line — you do not get 5 copies of "+
				"'text' for 1,5c|text, you get 1. Single-line addresses (5c|text), pattern addresses (|ptn|c|text), "+
				"and any '!'-inverted form instead replace one-for-one (one copy of 'text' per matched line). "+
				"Delimiter is always the literal '|' character — patterns/replacements must not contain a literal, "+
				"unescaped '|' (so alternation like 'a|b' cannot be used; use a character class '[ab]' or write two "+
				"separate commands instead). "+
				"Writes the file immediately unless dry_run=true, in which case nothing is written and a preview "+
				"summary is returned instead. "+
				"Supported command forms:\n"+
				"  s|ptn|repl|g        substitute all matches, whole file (ptn is Go RE2)\n"+
				"  s|ptn|repl          substitute first match per line, whole file\n"+
				"  2,5s|ptn|repl|g     substitute, restricted to lines 2-5 (use $ for last line, e.g. 5,$s|ptn|repl)\n"+
				"  |ptn|d              delete every line matching ptn (Go RE2)\n"+
				"  |ptn|!d             delete every line NOT matching ptn\n"+
				"  5d                  delete line 5 (no pattern involved)\n"+
				"  5!d                 delete every line except line 5\n"+
				"  1,5d                delete lines 1-5\n"+
				"  1,5!d               delete every line except lines 1-5\n"+
				"  5c|text             change (replace) line 5 with 'text'\n"+
				"  5!c|text            change every line except line 5 to 'text' (one copy per line)\n"+
				"  1,5c|text           change lines 1-5, COLLAPSED into a single line: 'text'\n"+
				"  1,5!c|text          change every line outside 1-5 to 'text' (one copy per line)\n"+
				"  |ptn|c|text         change every line matching ptn to 'text' (one-for-one, Go RE2)\n"+
				"  |ptn|!c|text        change every line NOT matching ptn to 'text' (one-for-one)\n"+
				"Unrecognized commands are rejected with an error rather than guessed at.",
		),
		mcp.WithString("file", mcp.Required(), mcp.Description("Path to the file to edit.")),
		mcp.WithString("command", mcp.Required(), mcp.Description(
			"The sed-lite command. 'ptn' fields are Go RE2 regex (same syntax as text_grep's pattern arg, "+
				"see pkg.go.dev/regexp/syntax) — NOT POSIX, NOT PCRE, no backreferences/lookaround. "+
				"Delimiter is always '|'. Examples: \"s|foo|bar|g\", \"s|^\\\\s+||\", \"|TODO|d\", \"1,5d\", \"5,$s|old|new\".",
		)),
		mcp.WithBoolean("dry_run", mcp.Description("If true, compute and describe the change but do not write the file. Default: false.")),
	), tool.handleSed)
}
