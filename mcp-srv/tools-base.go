package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	mcp "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	u "github.com/sunshine69/golang-tools/utils"
)

type BaseToolManager struct {
	AllowedTerminalCommandPattern string
	BlockedTerminalCommandPattern string
}

func (t *BaseToolManager) listDirectory(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := request.GetArguments()
	path := ""
	if p, ok := args["path"]; ok {
		path = fmt.Sprintf("%v", p)
	}
	cleanPath := filepath.Clean(path)
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
	if err := os.RemoveAll(cleanPath); err != nil {
		return mcp.NewToolResultText("[ERROR] " + err.Error()), err
	}

	return mcp.NewToolResultText("[OK]"), nil
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
	overwrite := false
	if o, ok := args["overwrite"]; ok {
		if bv, ok2 := o.(bool); ok2 {
			overwrite = bv
		}
	}

	cleanPath := filepath.Clean(path)
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

	workingDir := ""
	if wd, ok := args["working_dir"]; ok {
		workingDir = fmt.Sprintf("%v", wd)
	}
	confirmed := false
	if cf, ok := args["confirmed"]; ok {
		if bv, ok2 := cf.(bool); ok2 {
			confirmed = bv
		}
	}

	if !confirmed {
		return mcp.NewToolResultText(fmt.Sprintf(
			"⚠️  Confirmation required\n\nThe following command has NOT been executed yet:\n\n  %s\n\nTo run it, call run_terminal_command again with confirmed=true.",
			command,
		)), nil
	}

	cmd := exec.Command("sh", "-c", command)
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

	s.AddTool(mcp.NewTool("remove_directory",
		mcp.WithDescription("Remove directory in a given path. same as run unix command rm -rf <path>."),
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
		mcp.WithDescription(`Creates or overwrites a text file.

IMPORTANT:
- If the target file may already exist, you SHOULD set overwrite=true on the FIRST attempt.
- Do NOT first try overwrite=false and retry after failure unless preserving the existing file is required.
- Use overwrite=false only when you specifically want creation to fail if the file already exists.
`),
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
			mcp.DefaultBool(false),
			mcp.Description(`Whether to replace an existing file.

Set to true when updating, regenerating, or recreating files.
Set to false only when you explicitly need fail-if-exists behavior.`),
		),
	), t.createNewFile)

	s.AddTool(mcp.NewTool("run_terminal_command",
		mcp.WithDescription("Runs a shell command and returns its stdout and stderr. If the command does not return it will block you."),
		mcp.WithString("command", mcp.Required(), mcp.Description("The shell command to execute.")),
		mcp.WithString("working_dir", mcp.Description("Optional working directory for the command. Defaults to the server's current directory.")),
		mcp.WithBoolean("confirmed", mcp.DefaultBool(false), mcp.Description("Must be explicitly set to true to actually run the command. When false or omitted a confirmation message is returned and nothing is executed.")),
	), t.runTerminalCommand)

	s.AddTool(mcp.NewTool("file_glob_search",
		mcp.WithDescription("Searches for files matching a glob pattern under a root directory and returns their paths."),
		mcp.WithString("root", mcp.Required(), mcp.Description("The root directory to search within.")),
		mcp.WithString("pattern", mcp.Required(), mcp.Description("Glob pattern to match filenames (e.g. '*.go', '**/*.json').")),
		mcp.WithInteger("max_results", mcp.Description("Maximum number of results to return.")),
	), t.fileGlobSearch)

	s.AddTool(mcp.NewTool("find_replace_in_file",
		mcp.WithDescription("Performs a single find-and-replace operation in a file. Replaces the first (or all) occurrence(s) of a literal string or regex pattern with a replacement string."),
		mcp.WithString("path", mcp.Required(), mcp.Description("Path to the file to modify.")),
		mcp.WithString("find", mcp.Required(), mcp.Description("The string or regex pattern to search for.")),
		mcp.WithString("replace", mcp.Required(), mcp.Description("The replacement string. Supports $1, $2 … back-references when use_regex is true.")),
		mcp.WithBoolean("use_regex", mcp.DefaultBool(false), mcp.Description("Treat 'find' as a regular expression.")),
		mcp.WithBoolean("replace_all", mcp.DefaultBool(false), mcp.Description("Replace all occurrences instead of just the first.")),
	), t.findReplaceInFile)

	s.AddTool(mcp.NewTool("block_in_file",
		mcp.WithDescription("Replaces a multi-line block of text within a file using three regex anchor patterns that must appear in sequence. The tool searches the specified `path` for three lines matching the provided regular expressions in this order: first `start_block_ptn`, then `marker_ptn`, and finally `end_block_ptn`. These markers act as anchors; there can be any number of intermediate lines between them. Once found, the entire range starting from the line matching `start_block_ptn` through to the end of the line matching `end_block_ptn` is replaced by the string provided in `replace`. To be sure of accuracy all patterns must uniquely identify the block. Recommend full-line matching (use anchors ^ and $). If `end_block_ptn` contains EOF, reaching end-of-file counts as a match. `marker_ptn` is optional, give empty string if you want to bypass. On any error the function returns the error message"),
		mcp.WithString("path", mcp.Required(), mcp.Description("Path to the file to modify.")),
		mcp.WithString("start_block_ptn", mcp.Required(), mcp.Description("The regex pattern to match the start line of the block.")),
		mcp.WithString("end_block_ptn", mcp.Required(), mcp.Description("The regex pattern to match the end line of the block.")),
		mcp.WithString("marker_ptn", mcp.Required(), mcp.Description("The regex pattern to match in between the block.")),
		mcp.WithString("replace", mcp.Required(), mcp.Description("The replacement string.")),
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

func (t *BaseToolManager) blockInFile(_ context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := request.GetArguments()
	path := ""
	if p, ok := args["path"]; ok {
		path = fmt.Sprintf("%v", p)
	}
	upper_bound_ptn := []string{}
	if f, ok := args["start_block_ptn"]; ok {
		upper_bound_ptn = []string{f.(string)}
	}
	lower_bound_ptn := []string{}
	if f, ok := args["end_block_ptn"]; ok {
		lower_bound_ptn = []string{f.(string)}
	}
	marker_ptn := []string{}
	if f, ok := args["marker_ptn"]; ok {
		marker_ptn = []string{f.(string)}
	}
	replace := ""
	if r, ok := args["replace"]; ok {
		replace = fmt.Sprintf("%v", r)
	}

	cleanPath := filepath.Clean(path)

	output, start, _, _ := u.BlockInFile(cleanPath, upper_bound_ptn, lower_bound_ptn, marker_ptn, replace, false, false, 0)

	if start == -1 {
		return mcp.NewToolResultText("[ERROR] " + output), nil
	} else {
		return mcp.NewToolResultText("Success."), nil
	}
}
