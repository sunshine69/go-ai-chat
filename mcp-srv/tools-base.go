package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"

	mcp "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	u "github.com/sunshine69/golang-tools/utils"
)

type BaseToolManager struct {
	AllowedTerminalCommandPattern string
	BlockedTerminalCommandPattern string
	AllowedPathPattern            string // File path handling
	BlockedPathPattern            string
}

func (t *BaseToolManager) checkPath(path string) (*mcp.CallToolResult, error) {
	// Normalize separators so regex patterns written with '/' work on Windows too.
	normalizedPath := filepath.ToSlash(path)

	if t.AllowedPathPattern != "" {
		if !regexp.MustCompile(t.AllowedPathPattern).MatchString(normalizedPath) {
			return mcp.NewToolResultText("[ERROR]"), fmt.Errorf("[ERROR] denied access for path: '%s'. Allowed path pattern: '%s'", path, t.AllowedPathPattern)
		}
	}
	if t.BlockedPathPattern != "" {
		if regexp.MustCompile(t.BlockedPathPattern).MatchString(normalizedPath) {
			return mcp.NewToolResultText("[ERROR]"), fmt.Errorf("[ERROR] denied access for path: '%s'. Blocked path pattern: '%s'", path, t.BlockedPathPattern)
		}
	}
	return nil, nil
}

func (t *BaseToolManager) listDirectory(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := request.GetArguments()
	path := ""
	if p, ok := args["path"]; ok {
		path = fmt.Sprintf("%v", p)
	}
	cleanPath := filepath.Clean(path)
	if res, err := t.checkPath(cleanPath); err != nil {
		return res, err
	}
	entries, err := os.ReadDir(cleanPath)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Directory listing for: %s\n", cleanPath))
	sb.WriteString(strings.Repeat("-", 40) + "\n")
	for _, entry := range entries {
		icon := "📄 "
		if entry.IsDir() {
			icon = "📁 "
		}
		sb.WriteString(fmt.Sprintf("%s%s\n", icon, entry.Name()))
	}
	return mcp.NewToolResultText(sb.String()), nil
}

func (t *BaseToolManager) createDirectory(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := request.GetArguments()
	path := ""
	if p, ok := args["path"]; ok {
		path = fmt.Sprintf("%v", p)
	}
	cleanPath := filepath.Clean(path)
	if res, err := t.checkPath(cleanPath); err != nil {
		return res, err
	}
	if err := os.MkdirAll(cleanPath, 0o755); err != nil {
		return mcp.NewToolResultText("[ERROR] " + err.Error()), err
	}
	return mcp.NewToolResultText("[OK]"), nil
}

func (t *BaseToolManager) removeDirectory(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := request.GetArguments()
	path := ""
	if p, ok := args["path"]; ok {
		path = fmt.Sprintf("%v", p)
	}
	cleanPath := filepath.Clean(path)
	if res, err := t.checkPath(cleanPath); err != nil {
		return res, err
	}
	count := 0
	if strings.ContainsAny(cleanPath, `*?[]`) {
		matches, err := filepath.Glob(cleanPath)
		if err != nil {
			return mcp.NewToolResultText("[ERROR] " + err.Error()), err
		}
		for _, file := range matches {
			count++
			if err := os.RemoveAll(file); err != nil {
				return mcp.NewToolResultText("[ERROR] " + err.Error()), err
			}
		}
	} else {
		if err := os.RemoveAll(cleanPath); err != nil {
			return mcp.NewToolResultText("[ERROR] " + err.Error()), err
		}
	}
	return mcp.NewToolResultText(fmt.Sprintf("[OK] removed %d items", count)), nil
}

func (t *BaseToolManager) createNewFile(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := request.GetArguments()
	path := ""
	if p, ok := args["path"]; ok {
		path = fmt.Sprintf("%v", p)
	}
	content := ""
	if c, ok := args["content"]; ok {
		content = fmt.Sprintf("%v", c)
	}
	overwrite := true
	if o, ok := args["overwrite"]; ok {
		if bv, ok2 := o.(bool); ok2 {
			overwrite = bv
		}
	}

	cleanPath := filepath.Clean(path)
	if res, err := t.checkPath(cleanPath); err != nil {
		return res, err
	}
	if err := os.MkdirAll(filepath.Dir(cleanPath), 0755); err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	if !overwrite {
		if _, err := os.Stat(cleanPath); err == nil {
			return mcp.NewToolResultError(fmt.Sprintf("file already exists: %s (set overwrite=true to replace it)", cleanPath)), nil
		}
	}

	flags := os.O_WRONLY | os.O_CREATE | os.O_TRUNC
	f, err := os.OpenFile(cleanPath, flags, 0644)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	defer f.Close()

	if _, err := f.WriteString(content); err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	return mcp.NewToolResultText(fmt.Sprintf("File created successfully: %s (%d bytes)", cleanPath, len(content))), nil
}

func (t *BaseToolManager) runTerminalCommand(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := request.GetArguments()
	command := ""
	if c, ok := args["command"]; ok {
		command = fmt.Sprintf("%v", c)
	}
	if t.AllowedTerminalCommandPattern != "" {
		if !regexp.MustCompile(t.AllowedTerminalCommandPattern).MatchString(command) {
			return mcp.NewToolResultText("[ERROR]"), fmt.Errorf("[ERROR] command %s is denied. Only command matches the pattern '%s' will be allowed", command, t.AllowedTerminalCommandPattern)
		}
	}

	if t.BlockedTerminalCommandPattern != "" {
		if regexp.MustCompile(t.BlockedTerminalCommandPattern).MatchString(command) {
			return mcp.NewToolResultText("[ERROR]"), fmt.Errorf("[ERROR] command %s is denied. Command matches the pattern '%s' will be blocked", command, t.BlockedTerminalCommandPattern)
		}
	}
	// Parse the second part - it is the path and check it.
	cmdSlice := strings.Fields(command)
	if _, ok := unixFileTools[cmdSlice[0]]; ok {
		if len(cmdSlice) == 1 { // no good- these cmd requries to have the path
			return mcp.NewToolResultText("[ERROR]"), fmt.Errorf("[ERROR] command %s is denied. It needs to have the PATH as first argument.", command)
		}
		path := cmdSlice[1]
		if res, err := t.checkPath(path); err != nil {
			return res, err
		}
	}

	workingDir := "./"
	if wd, ok := args["working_dir"]; ok {
		workingDir = fmt.Sprintf("%v", wd)
	}
	if res, err := t.checkPath(workingDir); err != nil {
		return res, err
	}

	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.Command("cmd", "/C", command)
	} else {
		cmd = exec.Command("sh", "-c", command)
	}
	if workingDir != "" {
		cmd.Dir = filepath.Clean(workingDir)
	}

	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	runErr := cmd.Run()

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("$ %s\n", command))
	if stdout.Len() > 0 {
		sb.WriteString("\n--- stdout ---\n")
		sb.WriteString(stdout.String())
	}
	if stderr.Len() > 0 {
		sb.WriteString("\n--- stderr ---\n")
		sb.WriteString(stderr.String())
	}
	if runErr != nil {
		sb.WriteString(fmt.Sprintf("\n--- exit error ---\n%s\n", runErr.Error()))
	}
	if stdout.Len() == 0 && stderr.Len() == 0 && runErr == nil {
		sb.WriteString("(command completed with no output)\n")
	}

	return mcp.NewToolResultText(sb.String()), nil
}

func (t *BaseToolManager) fileGlobSearch(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := request.GetArguments()
	root := ""
	if r, ok := args["root"]; ok {
		root = fmt.Sprintf("%v", r)
	}
	pattern := ""
	if p, ok := args["pattern"]; ok {
		pattern = fmt.Sprintf("%v", p)
	}
	maxResults := 100
	if mr, ok := args["max_results"]; ok {
		switch v := mr.(type) {
		case float64:
			maxResults = int(v)
		case int:
			maxResults = v
		default:
			maxResults = 100
		}
	}

	cleanRoot := filepath.Clean(root)
	if res, err := t.checkPath(cleanRoot); err != nil {
		return res, err
	}
	namePattern := pattern
	recursive := false
	if strings.HasPrefix(pattern, "**/") {
		recursive = true
		namePattern = strings.TrimPrefix(pattern, "**/")
	}

	var matches []string
	err := filepath.WalkDir(cleanRoot, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		matched := false
		var matchErr error
		if recursive {
			matched, matchErr = filepath.Match(namePattern, d.Name())
		} else {
			rel, _ := filepath.Rel(cleanRoot, path)
			matched, matchErr = filepath.Match(pattern, rel)
		}
		if matchErr != nil {
			return fmt.Errorf("invalid glob pattern: %w", matchErr)
		}
		if matched {
			matches = append(matches, path)
			if len(matches) >= maxResults {
				return io.EOF
			}
		}
		return nil
	})

	if err != nil && err != io.EOF {
		return mcp.NewToolResultError(err.Error()), nil
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Found %d file(s) matching '%s' under %s", len(matches), pattern, cleanRoot))
	if len(matches) == maxResults {
		sb.WriteString(fmt.Sprintf(" (result capped at %d)", maxResults))
	}
	sb.WriteString(":\n")
	for _, m := range matches {
		sb.WriteString("  " + m + "\n")
	}

	return mcp.NewToolResultText(sb.String()), nil
}

func (t *BaseToolManager) findReplaceInFile(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := request.GetArguments()
	path := ""
	if p, ok := args["path"]; ok {
		path = fmt.Sprintf("%v", p)
	}
	find := ""
	if f, ok := args["find"]; ok {
		find = fmt.Sprintf("%v", f)
	}
	replace := ""
	if r, ok := args["replace"]; ok {
		replace = fmt.Sprintf("%v", r)
	}
	useRegex := false
	if u, ok := args["use_regex"]; ok {
		if bv, ok2 := u.(bool); ok2 {
			useRegex = bv
		}
	}
	replaceAll := false
	if ra, ok := args["replace_all"]; ok {
		if bv, ok2 := ra.(bool); ok2 {
			replaceAll = bv
		}
	}

	cleanPath := filepath.Clean(path)
	if res, err := t.checkPath(cleanPath); err != nil {
		return res, err
	}
	data, err := os.ReadFile(cleanPath)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	original := string(data)

	var updated string
	var count int

	if useRegex {
		re, compileErr := regexp.Compile(find)
		if compileErr != nil {
			return mcp.NewToolResultError(fmt.Sprintf("invalid regex pattern: %s", compileErr.Error())), nil
		}
		if replaceAll {
			count = len(re.FindAllString(original, -1))
			updated = re.ReplaceAllString(original, replace)
		} else {
			loc := re.FindStringIndex(original)
			if loc != nil {
				count = 1
				updated = original[:loc[0]] + re.ReplaceAllString(original[loc[0]:loc[1]], replace) + original[loc[1]:]
			} else {
				updated = original
			}
		}
	} else {
		if replaceAll {
			count = strings.Count(original, find)
			updated = strings.ReplaceAll(original, find, replace)
		} else {
			idx := strings.Index(original, find)
			if idx >= 0 {
				count = 1
				updated = original[:idx] + replace + original[idx+len(find):]
			} else {
				updated = original
			}
		}
	}

	if count == 0 {
		return mcp.NewToolResultText(fmt.Sprintf("Pattern not found in %s — file unchanged.", cleanPath)), nil
	}

	if err := os.WriteFile(cleanPath, []byte(updated), 0644); err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	noun := "occurrence"
	if count != 1 {
		noun = "occurrences"
	}
	return mcp.NewToolResultText(fmt.Sprintf("Replaced %d %s of %q in %s.", count, noun, find, cleanPath)), nil
}

func (t *BaseToolManager) fetchUrl(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := request.GetArguments()
	url := ""
	if u, ok := args["url"]; ok {
		url = fmt.Sprintf("%v", u)
	}
	if url == "" {
		return mcp.NewToolResultError("Missing or invalid 'url' parameter"), nil
	}

	markdownText, err := fetchAndConvert(url)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	docSize := len(markdownText)
	if docSize > 20000 {
		tempDir := u.Must(os.MkdirTemp("", "aig*"))
		tempFile := filepath.Join(tempDir, "fetch-url-doc.md")
		u.CheckErr(os.WriteFile(tempFile, []byte(markdownText), 0o644), "write doc file")
		return mcp.NewToolResultText(fmt.Sprintf("Document saved at file path '%s'. Size %d bytes. You can extract information from it using current available tools. Avoid reading the whole file with that size to avoid context overflow. Clean up the file after you no longer need it", tempFile, docSize)), nil
	}
	return mcp.NewToolResultText(markdownText), nil
}

func (t *BaseToolManager) httpRequest(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := request.GetArguments()
	method := args["method"].(string)
	url := args["url"].(string)
	body := ""
	if b, ok := args["body"].(string); ok {
		body = b
	}

	var req *http.Request
	var err error
	if body != "" {
		req, err = http.NewRequest(method, url, strings.NewReader(body))
	} else {
		req, err = http.NewRequest(method, url, nil)
	}
	if err != nil {
		return mcp.NewToolResultErrorFromErr("unable to create request", err), nil
	}

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return mcp.NewToolResultErrorFromErr("unable to execute request", err), nil
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(resp.Body)
		return mcp.NewToolResultError(fmt.Sprintf("HTTP %d: %s\nBody: %s", resp.StatusCode, resp.Status, string(respBody))), nil
	}

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return mcp.NewToolResultErrorFromErr("unable to read request response", err), nil
	}
	dataLen := len(respBody)
	if dataLen > 20000 {
		tmpDir, err := os.MkdirTemp("", "mcp-tool")
		if err != nil {
			return mcp.NewToolResultText("[ERROR] " + err.Error()), err
		}
		filePath := filepath.Join(tmpDir, "http-request-body.txt")
		if err := os.WriteFile(filePath, respBody, 0o640); err != nil {
			return mcp.NewToolResultText("[ERROR] " + err.Error()), err
		}
		return mcp.NewToolResultText(fmt.Sprintf("Status: %d\nSaved To: %s", resp.StatusCode, filePath)), nil
	}
	return mcp.NewToolResultText(fmt.Sprintf("Status: %d\nBody: %s", resp.StatusCode, string(respBody))), nil
}

func registerBaseTool(s *server.MCPServer, t *BaseToolManager) {
	// 	s.AddTool(mcp.NewTool("replace_lines_in_file",
	// 		mcp.WithDescription(`Replaces a range of lines in a file by 1-based line number.
	// Use when you know the exact line numbers from a previous text_head / text_section /
	// read_file output. start_line and end_line are both inclusive means IT WILL BE REPLACED. Set end_line to -1 to
	// replace from start_line to the end of the file.

	// The replacement text is written *VERBATIM* include a leading, trailing newline or spaces if you add them.
	// *IMPORTANT* to pay attention to the leading space at first line to maintain correct indentation with existing code
	// *ALWAYS* re read back after change to see changes and validate`),
	// 		mcp.WithString("path", mcp.Required(), mcp.Description("Path to the file to modify.")),
	// 		mcp.WithInteger("start_line", mcp.Required(), mcp.Description("First line to replace (1-based, inclusive).")),
	// 		mcp.WithInteger("end_line", mcp.Required(), mcp.Description("Last line to replace (1-based, inclusive). Pass -1 to replace through end of file.")),
	// 		mcp.WithString("replace", mcp.Required(), mcp.Description("Replacement text. Written verbatim in place of the removed lines.")),
	// 	), t.replaceLinesInFile)

	s.AddTool(mcp.NewTool("insert_text_to_file",
		mcp.WithDescription(`Insert a chunk of text  to a file BEFORE a 1-based line number.
	Use when you know the exact line numbers from a previous text_head / text_section /	read_file output. 
	Use 0 to insert at the beggining, and -1 or EOF to insert at the end of file.`),
		mcp.WithString("path", mcp.Required(), mcp.Description("Path to the file to modify.")),
		mcp.WithInteger("start_line", mcp.Required(), mcp.Description("First line to replace (1-based, inclusive).")),
		mcp.WithString("chunk", mcp.Required(), mcp.Description("Chunk of text to insert.")),
	), t.insertTextBlockToFile)

	s.AddTool(mcp.NewTool("fetch_url",
		mcp.WithDescription("Fetches a URL over HTTP and converts its HTML content into markdown text. If the content is larger than 20kb the content will be saved to a file and the result will be the file path so you can selectively read using grep using text tool"),
		mcp.WithString("url", mcp.Required(), mcp.Description("The URL to fetch and convert into markdown text")),
	), t.fetchUrl)

	s.AddTool(mcp.NewTool("list_directory",
		mcp.WithDescription("Lists the files and directories in a given path. Useful for understanding project structure."),
		mcp.WithString("path", mcp.Required(), mcp.Description("The directory path to list. Use '.' for the current directory.")),
	), t.listDirectory)

	s.AddTool(mcp.NewTool("create_directory",
		mcp.WithDescription("Create directory in a given path. same as run unix command mkdir -p <path>."),
		mcp.WithString("path", mcp.Required(), mcp.Description("The directory path to create.")),
	), t.createDirectory)

	s.AddTool(mcp.NewTool("remove_file_or_directory",
		mcp.WithDescription("Remove file or directory in a given path. same as run unix command rm -rf <path>. Wildcard in path accepted."),
		mcp.WithString("path", mcp.Required(), mcp.Description("The directory path to remove.")),
	), t.removeDirectory)

	s.AddTool(mcp.NewTool("read_file",
		mcp.WithDescription("Reads the content of a file from the local filesystem."),
		mcp.WithString("path", mcp.Required(), mcp.Description("The path to the file you want to read.")),
	), func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args := request.GetArguments()
		path := ""
		if p, ok := args["path"]; ok {
			path = fmt.Sprintf("%v", p)
		}
		content, err := readFileContent(path)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		return mcp.NewToolResultText(content), nil
	})
	s.AddTool(mcp.NewTool("create_new_file",
		mcp.WithDescription(`Creates or overwrites a text file. IMPORTANT - the file if exists will be overriden unless you set overwrite to false`),
		mcp.WithString(
			"path",
			mcp.Required(),
			mcp.Description("Absolute or relative file path to write."),
		),
		mcp.WithString(
			"content",
			mcp.Required(),
			mcp.Description("UTF-8 text content to write into the file."),
		),
		mcp.WithBoolean(
			"overwrite",
			mcp.DefaultBool(true),
			mcp.Description(`Whether to replace an existing file.

Set to true when updating, regenerating, or recreating files.
Set to false only when you explicitly need fail-if-exists behavior.`),
		),
	), t.createNewFile)

	s.AddTool(mcp.NewTool("run_terminal_command",
		mcp.WithDescription(`Runs a shell command and returns its stdout and stderr.

ABSOLUTE PATH IN COMMAND WILL BE DENIED. USE RELATIVE PATH TO CURRENT DIRECTORY. 
To run a command in a specific directory, use the working_dir argument otherwise it is defaulted to current working dir. If set, WORKING_DIR need to be relative path or match the path PATTERN return back to you.

  WRONG : command="cd /app && go build ./..."
  CORRECT: command="go build ./..."  working_dir="./app"

If the command does not return it will block you.`),
		mcp.WithString("command", mcp.Required(), mcp.Description("The shell command to execute. Must not use 'cd' — use working_dir instead.")),
		mcp.WithString("working_dir", mcp.Description("Directory to run the command in. Use this instead of 'cd'. Must be a relative path from the current directory.")),
		// mcp.WithBoolean("confirmed", mcp.DefaultBool(false), mcp.Description("Must be explicitly set to true to actually run the command. When false or omitted a confirmation message is returned and nothing is executed.")),
	), t.runTerminalCommand)

	s.AddTool(mcp.NewTool("file_glob_search",
		mcp.WithDescription("Searches for files matching a glob pattern under a root directory and returns their paths."),
		mcp.WithString("root", mcp.Required(), mcp.Description("The root directory to search within.")),
		mcp.WithString("pattern", mcp.Required(), mcp.Description("Glob pattern to match filenames (e.g. '*.go', '**/*.json').")),
		mcp.WithInteger("max_results", mcp.Description("Maximum number of results to return.")),
	), t.fileGlobSearch)

	s.AddTool(mcp.NewTool("find_replace_in_file",
		mcp.WithDescription(`Finds and replaces text within a file using either literal string matching or Go regex pattern matching. This is the go-to tool for most in-place edits — use it before reaching for block_in_file unless you specifically need multi-line anchor precision.

Use this tool when:
- You need to replace specific content (single-line or multi-line) without defining start/end anchors
- The replacement can be expressed as a single find/replace pair (even if using regex groups)
- You want simple, predictable behavior for common edits

Do NOT use block_in_file for this — prefer find_replace_in_file unless you need surgical anchor-based precision.

IMPORTANT: When use_regex=true, all patterns must use Go's regexp/syntax package syntax — NOT Perl/PCRE. Key differences:
- NO lookahead (?=...) or lookbehind (?<=...) assertions
- Recommend Using ^ and $ for full-line anchoring if needed
- Patterns do not need to match entire lines unless you add anchors

For simple literal text replacement, set use_regex=false (default) — the 'find' string will be matched exactly as written.`),
		mcp.WithString("path", mcp.Required(), mcp.Description("Absolute or relative path to the file. Use '.'-based paths like './src/main.py' — never absolute system paths.")),
		mcp.WithString("find", mcp.Required(), mcp.Description(`The text pattern to search for. When use_regex=false, this is a literal string (case-sensitive). When use_regex=true, this is a Go regex pattern. Examples: "func main()" (literal) or "^func [a-z]+\\(\\)" (regex)`)),
		mcp.WithString("replace", mcp.Required(), mcp.Description(`The replacement text. Supports $1, $2, etc. back-references when use_regex=true to refer to captured groups from the 'find' pattern. Example: "func ${1}() {" where $1 is a captured group. For multi-line replacements, include \\n in the string.`)),
		mcp.WithBoolean("use_regex", mcp.DefaultBool(false), mcp.Description(`Treat 'find' as a Go regex pattern instead of literal text. Set to true for flexible matching (e.g., replacing multiple similar patterns at once). When false, 'find' is matched exactly as written — no special regex characters are interpreted.`)),
		mcp.WithBoolean("replace_all", mcp.DefaultBool(false), mcp.Description(`When false, replaces only the first occurrence of 'find'. When true, replaces ALL occurrences in the file simultaneously. Use with caution to avoid unintended changes across unrelated parts of the file.`)),
	), t.findReplaceInFile)

	s.AddTool(mcp.NewTool("block_in_file",
		mcp.WithDescription(`Replaces a multi-line block of text within a file. This tool uses content anchors instead of line numbers, making it safe for iterative editing even after previous edits have shifted the file's structure.

Use this tool when:
- You need to replace, delete, or insert a specific section of code/text
- You know unique text on both the start and end lines of the block to target
- You want to avoid the "line number drift" problem that occurs with line-number-based tools

Do NOT use this for simple single-line replacements. 
DO NOT give start_block_string and end_block_string are the same — use find_replace_in_file instead.

How it works:

If marker_string is NOT provided
1. Find the START line matching start_block_string (the opening anchor)
2. Find the END line matching end_block_string (the closing anchor)

If marker_string IS PROVIDED (THE ABSOLUTE ACCURACY CASE)
1. Match marker_string (an intermediate pattern, between start and end to confirm accuracy)
2. Search START line by matching for start_block_string upward from the marker position. If found then
3. Search END line by matching for end_block_string downward from marker position. If found then

Replace everything from the START line through to (and including) the END line with 'replace'

IMPORTANT: All patterns are RAW string.
- IF PATTERN start_block_string length shorter than 5 chars will be rejected

**WARNING**: If your start or end pattern could match multiple lines in the file, you MUST use
 'marker_string' to disambiguate. For example, if replacing an HTML '<section>' when there are two 
sections, include a unique line from inside that section as the marker to ensure only the correct block is replaced.

If end_block_string ends with EOF, reaching the end of file counts as a match (useful for appending to files).

marker_string is optional: pass an empty string "" to skip it and only use start+end anchors.

**TIP**
- To be 100% sure, you can generate your own pattern markers
  - Use insert_text_to_file at a line with your own text string for start
  - repeat it for the end 
  - Call block_in_file with your own pattern marker
`),
		mcp.WithString("path", mcp.Required(), mcp.Description("Absolute or relative path to the file. Use '.'-based paths like './src/main.py' — never absolute system paths.")),
		mcp.WithString("start_block_string", mcp.Required(), mcp.Description(`Go regex pattern for the first line of the block to replace. Must match the ENTIRE line. Use anchors: ^ and $ for full-line matching. Example: "^func main() {" or "^[ \\t]*// TODO:". NO lookahead/lookbehind — Go does not support (?=...) or (?<=...).`)),
		mcp.WithString("end_block_string", mcp.Required(), mcp.Description(`Go regex pattern for the last line of the block to replace. Must match the ENTIRE line. Use anchors: ^ and $ for full-line matching. Example: "^}$" or "^[ \\t]*// END TODO$". To append content (match end-of-file), use "EOF"`)),
		mcp.WithString("marker_string", mcp.Description(`Optional Go regex pattern for an intermediate line within the block. Pass "" (empty string) to skip this step and only match start+end lines. If provided, must match the ENTIRE line with ^/$ anchors. Example: "^[ \\t]*// middle marker$". NO lookahead/lookbehind — Go does not support (?=...) or (?<=...).`)),
		mcp.WithString("replace", mcp.Required(), mcp.Description(`The replacement text. Replace will completely overwrite everything from start_block_string through end_block_string (inclusive). Use \n for newlines within the replacement string.`)),
	), t.blockInFile)

	s.AddTool(mcp.NewTool("http_request",
		mcp.WithDescription("Make HTTP requests to external APIs. If response is greater 20Kb it will be saved into a file and file path will be returned."),
		mcp.WithString("method",
			mcp.Required(),
			mcp.Description("HTTP method to use"),
			mcp.Enum("GET", "POST", "PUT", "DELETE"),
		),
		mcp.WithString("url",
			mcp.Required(),
			mcp.Description("URL to send the request to"),
			mcp.Pattern("^https?://.*"),
		),
		mcp.WithString("body",
			mcp.Description("Request body (for POST/PUT)"),
		),
	), t.httpRequest)
}

// InsertTextBlock inserts a chunk of text into a file at a 1-based line number.
// Position must be "before" or "after". Pass -1 to automatically insert at the end of the file.
func InsertTextBlock(filePath string, lineNumber int, textBlock string, position string) error {
	// 1. Validate position input
	position = strings.ToLower(position)
	if position != "before" && position != "after" {
		return fmt.Errorf("invalid position %q: must be 'before' or 'after'", position)
	}

	// 2. Open the file for reading
	file, err := os.Open(filePath)
	if err != nil {
		return fmt.Errorf("failed to open file: %w", err)
	}

	// Read file line by line into a slice
	var lines []string
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
	}
	file.Close() // Close early before rewriting

	if err := scanner.Err(); err != nil {
		return fmt.Errorf("failed to read file contents: %w", err)
	}

	// 3. Normalize the text block
	textBlock = strings.TrimSuffix(textBlock, "\n")

	// 4. Calculate the 0-based insertion index
	var insertIndex int
	if lineNumber == -1 {
		// Explicit AI instruction to target the end of the file
		insertIndex = len(lines)
	} else if position == "before" {
		insertIndex = lineNumber - 1
	} else {
		insertIndex = lineNumber
	}

	// 5. Bound the index safely to prevent runtime panics
	if insertIndex < 0 {
		insertIndex = 0
	}
	if insertIndex > len(lines) {
		insertIndex = len(lines)
	}

	// 6. Perform the slice insertion
	updatedLines := make([]string, 0, len(lines)+1)
	updatedLines = append(updatedLines, lines[:insertIndex]...)
	updatedLines = append(updatedLines, textBlock)
	updatedLines = append(updatedLines, lines[insertIndex:]...)

	// 7. Write the updated content back to the file
	output, err := os.OpenFile(filePath, os.O_WRONLY|os.O_TRUNC, 0644)
	if err != nil {
		return fmt.Errorf("failed to open file for writing: %w", err)
	}
	defer output.Close()

	writer := bufio.NewWriter(output)
	for _, line := range updatedLines {
		if _, err := writer.WriteString(line + "\n"); err != nil {
			return fmt.Errorf("failed to write line: %w", err)
		}
	}

	return writer.Flush()
}
func (t BaseToolManager) insertTextBlockToFile(_ context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := request.GetArguments()
	path := ""
	if p, ok := args["path"]; ok {
		path = fmt.Sprintf("%v", p)
	}
	startLine := 0
	if v, ok := args["start_line"]; ok {
		switch n := v.(type) {
		case float64:
			startLine = int(n)
		case int:
			startLine = n
		case string:
			switch n {
			case "BOF":
				startLine = 0
			case "EOF":
				startLine = -1
			}
		}
	}
	replace := ""
	if r, ok := args["chunk"]; ok {
		replace = r.(string)
	}

	cleanPath := filepath.Clean(path)
	if res, err := t.checkPath(cleanPath); err != nil {
		return res, err
	}

	if err := InsertTextBlock(cleanPath, startLine, replace, "before"); err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	return mcp.NewToolResultText("[OK]"), nil
}

func (t *BaseToolManager) blockInFile(_ context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := request.GetArguments()
	path := ""
	if p, ok := args["path"]; ok {
		path = fmt.Sprintf("%v", p)
	}
	upper_bound_ptn := []string{}
	if f, ok := args["start_block_string"]; ok {
		upper_bound_ptn = []string{regexp.QuoteMeta(f.(string))}
	}
	lower_bound_ptn := []string{}
	if f, ok := args["end_block_string"]; ok {
		lower_bound_ptn = []string{regexp.QuoteMeta(f.(string))}
	}
	if len(upper_bound_ptn[0]) < 5 {
		return mcp.NewToolResultError("[ERROR] the upper_bound_ptn regex pattern is shorter than 5"), nil
	}
	marker_string := []string{}
	if f, ok := args["marker_string"]; ok {
		if v, ok1 := f.(string); ok1 && v != "" {
			marker_string = []string{regexp.QuoteMeta(v)}
		}
	}
	if len(marker_string) < 5 && len(lower_bound_ptn) < 5 {
		return mcp.NewToolResultError("[ERROR] BOTH lower_bound_ptn and marker_string shorter than 5"), nil
	}
	replace := ""
	if r, ok := args["replace"]; ok {
		replace = fmt.Sprintf("%v", r)
	}

	cleanPath := filepath.Clean(path)
	if res, err := t.checkPath(cleanPath); err != nil {
		return res, err
	}
	filename := filepath.Base(cleanPath)
	backupfile := filepath.Join(filepath.Dir(cleanPath), filename+".bak")
	defer os.RemoveAll(backupfile)

	output, start, _, _ := u.BlockInFile(cleanPath, upper_bound_ptn, lower_bound_ptn, marker_string, replace, false, true, 0)

	if start == -1 {
		if u.FileExistsV2(backupfile) == nil {
			if err := u.Copy(backupfile, cleanPath); err != nil {
				return mcp.NewToolResultError("[ERROR] Hit error but we can not restore backup file. Error: " + err.Error() + "\nOriginal error: " + output), nil
			}
		}
		return mcp.NewToolResultText(output), nil
	} else {
		return mcp.NewToolResultText("OK REPLACED OLD BLOCK:\n" + output), nil
	}
}

func (t *BaseToolManager) replaceLinesInFile(_ context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := request.GetArguments()
	path := ""
	if p, ok := args["path"]; ok {
		path = fmt.Sprintf("%v", p)
	}
	startLine := 0
	if v, ok := args["start_line"]; ok {
		switch n := v.(type) {
		case float64:
			startLine = int(n)
		case int:
			startLine = n
		}
	}
	endLine := -1
	if v, ok := args["end_line"]; ok {
		switch n := v.(type) {
		case float64:
			endLine = int(n)
		case int:
			endLine = n
		}
	}
	replace := ""
	if r, ok := args["replace"]; ok {
		replace = r.(string)
	}

	cleanPath := filepath.Clean(path)
	if res, err := t.checkPath(cleanPath); err != nil {
		return res, err
	}

	data, err := os.ReadFile(cleanPath)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	lines := strings.Split(string(data), "\n")
	total := len(lines)

	// Convert 1-based inclusive line numbers to 0-based slice indices.
	start := startLine - 1
	end := endLine // endLine == -1 means EOF; otherwise it's 1-based inclusive → slice end = endLine
	if endLine == -1 {
		end = total
	}

	if start < 0 || start >= total {
		return mcp.NewToolResultError(fmt.Sprintf("start_line %d out of range (file has %d lines)", startLine, total)), nil
	}
	if end < start || end > total {
		return mcp.NewToolResultError(fmt.Sprintf("end_line %d out of range (file has %d lines)", endLine, total)), nil
	}

	var head, tail []string
	head = lines[:start]
	tail = lines[end:]

	var sb strings.Builder
	if len(head) > 0 {
		sb.WriteString(strings.Join(head, "\n"))
		sb.WriteByte('\n')
	}

	sb.WriteString(replace)
	if len(tail) > 0 {
		sb.WriteString(strings.Join(tail, "\n"))
	}
	updated := sb.String()

	if err := os.WriteFile(cleanPath, []byte(updated), 0644); err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	noun := "line"
	replaced := end - start
	if endLine == -1 {
		replaced = total - start
	}
	if replaced != 1 {
		noun = "lines"
	}
	return mcp.NewToolResultText(fmt.Sprintf("Replaced %d %s (%d–%s) in %s.", replaced, noun, startLine, func() string {
		if endLine == -1 {
			return "EOF"
		}
		return fmt.Sprintf("%d", endLine)
	}(), cleanPath)), nil
}
