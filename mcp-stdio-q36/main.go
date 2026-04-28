package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	html2md "github.com/JohannesKaufmann/html-to-markdown"
)

// =============================================================================
// JSON-RPC 2.0 & MCP Structures
// =============================================================================

type Request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      interface{}     `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type Response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      interface{}     `json:"id"`
	Result  *ResponseResult `json:"result,omitempty"`
	Error   *ErrorResponse  `json:"error,omitempty"`
}

type ResponseResult struct {
	ProtocolVersion string                 `json:"protocolVersion,omitempty"`
	Capabilities    map[string]interface{} `json:"capabilities,omitempty"`
	ServerInfo      map[string]string      `json:"serverInfo,omitempty"`
	Tools           []Tool                 `json:"tools,omitempty"`
	Content         []ContentBlock         `json:"content,omitempty"`
}

type ErrorResponse struct {
	Code    int         `json:"code"`
	Message string      `json:"message"`
	Data    interface{} `json:"data,omitempty"`
}

// MCP Tool Definition
type Tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

// MCP Content Block
type ContentBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

const mcpVersion = "2024-11-05"

// =============================================================================
// Core Server Logic
// =============================================================================

func main() {
	scanner := bufio.NewScanner(os.Stdin)
	// Increase scanner buffer for large file contents
	buf := make([]byte, 10*1024*1024)
	scanner.Buffer(buf, cap(buf))

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		var req Request
		if err := json.Unmarshal(line, &req); err != nil {
			writeResponse(req.ID, nil, &ErrorResponse{Code: -32700, Message: "Parse error"})
			continue
		}

		switch req.Method {
		case "initialize":
			handleInitialize(&req)
		case "tools/list":
			handleToolsList(&req)
		case "tools/call":
			handleToolCall(&req)
		default:
			writeResponse(req.ID, nil, &ErrorResponse{Code: -32601, Message: "Method not found"})
		}
	}

	if err := scanner.Err(); err != nil {
		fmt.Fprintf(os.Stderr, "stdin error: %v\n", err)
		os.Exit(1)
	}
}

func writeResponse(id interface{}, result *ResponseResult, err *ErrorResponse) {
	resp := Response{
		JSONRPC: "2.0",
		ID:      id,
	}
	if result != nil {
		resp.Result = result
	} else {
		resp.Error = err
	}

	out, marshalErr := json.Marshal(resp)
	if marshalErr != nil {
		fmt.Fprintf(os.Stderr, "marshal error: %v\n", marshalErr)
		return
	}
	fmt.Fprintln(os.Stdout, string(out))
}

func handleInitialize(req *Request) {
	writeResponse(req.ID, &ResponseResult{
		ProtocolVersion: mcpVersion,
		Capabilities:    map[string]interface{}{"tools": struct{}{}},
		ServerInfo:      map[string]string{"name": "mcp-fetch-server", "version": "1.0.0"},
	}, nil)
}

func handleToolsList(req *Request) {
	tools := []Tool{
		{
			Name:        "fetch_url",
			Description: "Fetches a URL over HTTP and converts its HTML content into markdown text.",
			InputSchema: json.RawMessage(`{
				"type": "object",
				"properties": {
					"url": {
						"type": "string",
						"description": "The URL to fetch and convert into markdown text"
					}
				},
				"required": ["url"]
			}`),
		},
		{
			Name:        "list_directory",
			Description: "Lists the files and directories in a given path. Useful for understanding project structure.",
			InputSchema: json.RawMessage(`{
				"type": "object",
				"properties": {
					"path": {
						"type": "string",
						"description": "The directory path to list. Use '.' for the current directory."
					}
				},
				"required": ["path"]
			}`),
		},
		{
			Name:        "read_file",
			Description: "Reads the content of a file from the local filesystem.",
			InputSchema: json.RawMessage(`{
				"type": "object",
				"properties": {
					"path": {
						"type": "string",
						"description": "The path to the file you want to read."
					}
				},
				"required": ["path"]
			}`),
		},
		{
			Name:        "create_new_file",
			Description: "Creates a new file at the given path with the provided content. Fails if the file already exists unless overwrite is set to true. Parent directories are created automatically.",
			InputSchema: json.RawMessage(`{
				"type": "object",
				"properties": {
					"path": {
						"type": "string",
						"description": "The path where the new file should be created."
					},
					"content": {
						"type": "string",
						"description": "The text content to write into the file."
					},
					"overwrite": {
						"type": "boolean",
						"description": "If true, overwrite the file if it already exists. Defaults to false.",
						"default": false
					}
				},
				"required": ["path", "content"]
			}`),
		},
		{
			Name:        "run_terminal_command",
			Description: "Runs a shell command and returns its stdout and stderr. Requires the caller to pass confirmed=true to acknowledge they want to execute the command; if confirmed is false or omitted the command is NOT run and a confirmation prompt is returned instead.",
			InputSchema: json.RawMessage(`{
				"type": "object",
				"properties": {
					"command": {
						"type": "string",
						"description": "The shell command to execute."
					},
					"working_dir": {
						"type": "string",
						"description": "Optional working directory for the command. Defaults to the server's current directory."
					},
					"confirmed": {
						"type": "boolean",
						"description": "Must be explicitly set to true to actually run the command. When false or omitted a confirmation message is returned and nothing is executed.",
						"default": false
					}
				},
				"required": ["command"]
			}`),
		},
		{
			Name:        "file_glob_search",
			Description: "Searches for files matching a glob pattern under a root directory and returns their paths.",
			InputSchema: json.RawMessage(`{
				"type": "object",
				"properties": {
					"root": {
						"type": "string",
						"description": "The root directory to search within."
					},
					"pattern": {
						"type": "string",
						"description": "Glob pattern to match filenames (e.g. '*.go', '**/*.json'). The pattern is matched against each file's path relative to root."
					},
					"max_results": {
						"type": "integer",
						"description": "Maximum number of results to return. Defaults to 100.",
						"default": 100
					}
				},
				"required": ["root", "pattern"]
			}`),
		},
		{
			Name:        "find_replace_in_file",
			Description: "Performs a single find-and-replace operation in a file. Replaces the first (or all) occurrence(s) of a literal string or regex pattern with a replacement string.",
			InputSchema: json.RawMessage(`{
				"type": "object",
				"properties": {
					"path": {
						"type": "string",
						"description": "Path to the file to modify."
					},
					"find": {
						"type": "string",
						"description": "The string or regex pattern to search for."
					},
					"replace": {
						"type": "string",
						"description": "The replacement string. Supports $1, $2 … back-references when use_regex is true."
					},
					"use_regex": {
						"type": "boolean",
						"description": "Treat 'find' as a regular expression. Defaults to false (literal string match).",
						"default": false
					},
					"replace_all": {
						"type": "boolean",
						"description": "Replace all occurrences instead of just the first. Defaults to false.",
						"default": false
					}
				},
				"required": ["path", "find", "replace"]
			}`),
		},
	}

	writeResponse(req.ID, &ResponseResult{
		Tools: tools,
	}, nil)
}

func handleToolCall(req *Request) {
	var args struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments,omitempty"`
	}
	if err := json.Unmarshal(req.Params, &args); err != nil {
		writeResponse(req.ID, nil, &ErrorResponse{Code: -32602, Message: "Invalid params"})
		return
	}

	switch args.Name {
	// ── Existing tools ─────────────────────────────────────────────────────────
	case "fetch_url":
		var params struct {
			URL string `json:"url"`
		}
		if err := json.Unmarshal(args.Arguments, &params); err != nil || strings.TrimSpace(params.URL) == "" {
			writeResponse(req.ID, nil, &ErrorResponse{Code: -32602, Message: "Missing or invalid 'url' parameter"})
			return
		}
		markdownText, err := fetchAndConvert(params.URL)
		if err != nil {
			writeResponse(req.ID, nil, &ErrorResponse{Code: -32603, Message: err.Error()})
			return
		}
		writeResponse(req.ID, &ResponseResult{
			Content: []ContentBlock{{Type: "text", Text: markdownText}},
		}, nil)

	case "list_directory":
		var params struct {
			Path string `json:"path"`
		}
		if err := json.Unmarshal(args.Arguments, &params); err != nil || params.Path == "" {
			writeResponse(req.ID, nil, &ErrorResponse{Code: -32602, Message: "Missing or invalid 'path' parameter"})
			return
		}
		dirListing, err := listDirectory(params.Path)
		if err != nil {
			writeResponse(req.ID, nil, &ErrorResponse{Code: -32603, Message: err.Error()})
			return
		}
		writeResponse(req.ID, &ResponseResult{
			Content: []ContentBlock{{Type: "text", Text: dirListing}},
		}, nil)

	case "read_file":
		var params struct {
			Path string `json:"path"`
		}
		if err := json.Unmarshal(args.Arguments, &params); err != nil || params.Path == "" {
			writeResponse(req.ID, nil, &ErrorResponse{Code: -32602, Message: "Missing or invalid 'path' parameter"})
			return
		}
		fileContent, err := readFileContent(params.Path)
		if err != nil {
			writeResponse(req.ID, nil, &ErrorResponse{Code: -32603, Message: err.Error()})
			return
		}
		writeResponse(req.ID, &ResponseResult{
			Content: []ContentBlock{{Type: "text", Text: fileContent}},
		}, nil)

	// ── New tools ──────────────────────────────────────────────────────────────
	case "create_new_file":
		var params struct {
			Path      string `json:"path"`
			Content   string `json:"content"`
			Overwrite bool   `json:"overwrite"`
		}
		if err := json.Unmarshal(args.Arguments, &params); err != nil || params.Path == "" {
			writeResponse(req.ID, nil, &ErrorResponse{Code: -32602, Message: "Missing or invalid parameters for create_new_file"})
			return
		}
		result, err := createNewFile(params.Path, params.Content, params.Overwrite)
		if err != nil {
			writeResponse(req.ID, nil, &ErrorResponse{Code: -32603, Message: err.Error()})
			return
		}
		writeResponse(req.ID, &ResponseResult{
			Content: []ContentBlock{{Type: "text", Text: result}},
		}, nil)

	case "run_terminal_command":
		var params struct {
			Command    string `json:"command"`
			WorkingDir string `json:"working_dir"`
			Confirmed  bool   `json:"confirmed"`
		}
		if err := json.Unmarshal(args.Arguments, &params); err != nil || strings.TrimSpace(params.Command) == "" {
			writeResponse(req.ID, nil, &ErrorResponse{Code: -32602, Message: "Missing or invalid 'command' parameter"})
			return
		}
		result, err := runTerminalCommand(params.Command, params.WorkingDir, params.Confirmed)
		if err != nil {
			writeResponse(req.ID, nil, &ErrorResponse{Code: -32603, Message: err.Error()})
			return
		}
		writeResponse(req.ID, &ResponseResult{
			Content: []ContentBlock{{Type: "text", Text: result}},
		}, nil)

	case "file_glob_search":
		var params struct {
			Root       string `json:"root"`
			Pattern    string `json:"pattern"`
			MaxResults int    `json:"max_results"`
		}
		if err := json.Unmarshal(args.Arguments, &params); err != nil || params.Root == "" || params.Pattern == "" {
			writeResponse(req.ID, nil, &ErrorResponse{Code: -32602, Message: "Missing or invalid parameters for file_glob_search"})
			return
		}
		if params.MaxResults <= 0 {
			params.MaxResults = 100
		}
		result, err := fileGlobSearch(params.Root, params.Pattern, params.MaxResults)
		if err != nil {
			writeResponse(req.ID, nil, &ErrorResponse{Code: -32603, Message: err.Error()})
			return
		}
		writeResponse(req.ID, &ResponseResult{
			Content: []ContentBlock{{Type: "text", Text: result}},
		}, nil)

	case "find_replace_in_file":
		var params struct {
			Path       string `json:"path"`
			Find       string `json:"find"`
			Replace    string `json:"replace"`
			UseRegex   bool   `json:"use_regex"`
			ReplaceAll bool   `json:"replace_all"`
		}
		if err := json.Unmarshal(args.Arguments, &params); err != nil || params.Path == "" || params.Find == "" {
			writeResponse(req.ID, nil, &ErrorResponse{Code: -32602, Message: "Missing or invalid parameters for find_replace_in_file"})
			return
		}
		result, err := findReplaceInFile(params.Path, params.Find, params.Replace, params.UseRegex, params.ReplaceAll)
		if err != nil {
			writeResponse(req.ID, nil, &ErrorResponse{Code: -32603, Message: err.Error()})
			return
		}
		writeResponse(req.ID, &ResponseResult{
			Content: []ContentBlock{{Type: "text", Text: result}},
		}, nil)

	default:
		writeResponse(req.ID, nil, &ErrorResponse{Code: -32601, Message: fmt.Sprintf("Tool '%s' not found", args.Name)})
	}
}

// =============================================================================
// Tool Implementations
// =============================================================================

// readFileContent reads the content of a file and returns it as a string.
func readFileContent(targetPath string) (string, error) {
	cleanPath := filepath.Clean(targetPath)
	content, err := os.ReadFile(cleanPath)
	if err != nil {
		return "", fmt.Errorf("failed to read file: %w", err)
	}
	return string(content), nil
}

// listDirectory reads the filesystem and returns a formatted string.
func listDirectory(targetPath string) (string, error) {
	cleanPath := filepath.Clean(targetPath)
	entries, err := os.ReadDir(cleanPath)
	if err != nil {
		return "", fmt.Errorf("failed to read directory: %w", err)
	}
	if len(entries) == 0 {
		return fmt.Sprintf("Directory: %s (empty)", cleanPath), nil
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
	return sb.String(), nil
}

// createNewFile creates a file at path with content.
// If overwrite is false and the file exists, it returns an error.
func createNewFile(path, content string, overwrite bool) (string, error) {
	cleanPath := filepath.Clean(path)

	// Create parent directories if needed.
	if err := os.MkdirAll(filepath.Dir(cleanPath), 0755); err != nil {
		return "", fmt.Errorf("failed to create parent directories: %w", err)
	}

	// Check existence when overwrite is disabled.
	if !overwrite {
		if _, err := os.Stat(cleanPath); err == nil {
			return "", fmt.Errorf("file already exists: %s (set overwrite=true to replace it)", cleanPath)
		}
	}

	flags := os.O_WRONLY | os.O_CREATE | os.O_TRUNC
	f, err := os.OpenFile(cleanPath, flags, 0644)
	if err != nil {
		return "", fmt.Errorf("failed to create file: %w", err)
	}
	defer f.Close()

	if _, err := f.WriteString(content); err != nil {
		return "", fmt.Errorf("failed to write content: %w", err)
	}

	return fmt.Sprintf("File created successfully: %s (%d bytes)", cleanPath, len(content)), nil
}

// runTerminalCommand executes a shell command if confirmed=true,
// otherwise returns a human-readable confirmation prompt.
func runTerminalCommand(command, workingDir string, confirmed bool) (string, error) {
	if !confirmed {
		msg := fmt.Sprintf(
			"⚠️  Confirmation required\n\nThe following command has NOT been executed yet:\n\n  %s\n\nTo run it, call run_terminal_command again with confirmed=true.",
			command,
		)
		return msg, nil
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

	return sb.String(), nil
}

// fileGlobSearch walks root and returns paths of files whose names match pattern.
// It supports the standard filepath.Match syntax as well as a leading **/ prefix
// (which makes the pattern match anywhere in the tree).
func fileGlobSearch(root, pattern string, maxResults int) (string, error) {
	cleanRoot := filepath.Clean(root)

	// Detect "**/" prefix and strip it so we do a recursive name match.
	recursive := false
	namePattern := pattern
	if strings.HasPrefix(pattern, "**/") {
		recursive = true
		namePattern = strings.TrimPrefix(pattern, "**/")
	}

	var matches []string

	err := filepath.WalkDir(cleanRoot, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil // skip unreadable entries
		}
		if d.IsDir() {
			return nil
		}

		var matched bool
		var matchErr error

		if recursive {
			// Match only the file name portion against the pattern.
			matched, matchErr = filepath.Match(namePattern, d.Name())
		} else {
			// Match the relative path from root against the full pattern.
			rel, _ := filepath.Rel(cleanRoot, path)
			matched, matchErr = filepath.Match(pattern, rel)
		}

		if matchErr != nil {
			return fmt.Errorf("invalid glob pattern: %w", matchErr)
		}
		if matched {
			matches = append(matches, path)
			if len(matches) >= maxResults {
				return io.EOF // signal early exit
			}
		}
		return nil
	})

	// io.EOF is our early-exit sentinel, not a real error.
	if err != nil && err != io.EOF {
		return "", fmt.Errorf("walk error: %w", err)
	}

	if len(matches) == 0 {
		return fmt.Sprintf("No files found matching pattern '%s' under %s", pattern, cleanRoot), nil
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
	return sb.String(), nil
}

// findReplaceInFile performs a find-and-replace on the contents of a file.
func findReplaceInFile(path, find, replace string, useRegex, replaceAll bool) (string, error) {
	cleanPath := filepath.Clean(path)

	data, err := os.ReadFile(cleanPath)
	if err != nil {
		return "", fmt.Errorf("failed to read file: %w", err)
	}
	original := string(data)

	var updated string
	var count int

	if useRegex {
		re, err := regexp.Compile(find)
		if err != nil {
			return "", fmt.Errorf("invalid regex pattern: %w", err)
		}
		if replaceAll {
			matches := re.FindAllString(original, -1)
			count = len(matches)
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
		return fmt.Sprintf("Pattern not found in %s — file unchanged.", cleanPath), nil
	}

	if err := os.WriteFile(cleanPath, []byte(updated), 0644); err != nil {
		return "", fmt.Errorf("failed to write file: %w", err)
	}

	noun := "occurrence"
	if count != 1 {
		noun = "occurrences"
	}
	return fmt.Sprintf("Replaced %d %s of %q in %s.", count, noun, find, cleanPath), nil
}

func fetchAndConvert(url string) (string, error) {
	client := &http.Client{}
	httpReq, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return "", fmt.Errorf("invalid URL: %w", err)
	}
	httpReq.Header.Set("User-Agent", "MCP-Stdio-Server/1.0")

	resp, err := client.Do(httpReq)
	if err != nil {
		return "", fmt.Errorf("fetch failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP error: %d %s", resp.StatusCode, resp.Status)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("read body failed: %w", err)
	}

	return HTMLToMarkdown(body)
}

func HTMLToMarkdown(htmlb []byte) (string, error) {
	html := string(htmlb)
	if strings.TrimSpace(html) == "" {
		return "", nil
	}

	converter := html2md.NewConverter("", true, nil)
	md, err := converter.ConvertString(html)
	if err != nil {
		return "", fmt.Errorf("html to md: %w", err)
	}

	return strings.TrimSpace(md), nil
}

// htmlToMarkdown is a stdlib-only fallback (strips tags, preserves block structure).
func htmlToMarkdown(html []byte) string {
	text := string(html)

	reBlock := regexp.MustCompile(`(?is)<script[^>]*>.*?</script>|<style[^>]*>.*?</style>`)
	text = reBlock.ReplaceAllString(text, "")

	reTag := regexp.MustCompile(`</?[^>]+>`)
	text = reTag.ReplaceAllString(text, " ")

	blockRe := regexp.MustCompile(`<br\s*/?>|<p>|</p>|<div>|</div>|<h[1-6]>|</h[1-6]>`)
	text = blockRe.ReplaceAllString(text, "\n")

	reWS := regexp.MustCompile(`\s+`)
	text = reWS.ReplaceAllString(text, " ")

	return strings.TrimSpace(text)
}
