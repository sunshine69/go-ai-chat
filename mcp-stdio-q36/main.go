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
	"time"

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
	Resources       []Resource             `json:"resources,omitempty"`
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

// MCP Resource Definition
type Resource struct {
	URI         string `json:"uri"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	MIMEType    string `json:"mimeType,omitempty"`
}

const mcpVersion = "2024-11-05"

// =============================================================================
// Resource helpers (walks ./aidocs/ and registers .md files)
// =============================================================================
func registerDirectoryResources(dirPath, description string) ([]Resource, error) {
	cleanDir := filepath.Clean(dirPath)
	if _, err := os.Stat(cleanDir); os.IsNotExist(err) {
		return nil, fmt.Errorf("directory not found: %s", cleanDir)
	}

	var resources []Resource
	err := filepath.WalkDir(cleanDir, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil || d.IsDir() {
			return nil // skip unreadable entries and directories
		}
		baseName := filepath.Base(path)
		ext := strings.ToLower(filepath.Ext(baseName))

		var mimeType string
		switch ext {
		case ".md":
			mimeType = "text/markdown"
		case ".txt":
			mimeType = "text/plain"
		case ".json", ".yaml", ".yml":
			mimeType = "application/json"
		case ".xml":
			mimeType = "application/xml"
		case ".html", ".htm":
			mimeType = "text/html"
		case ".css":
			mimeType = "text/css"
		case ".js", ".ts", ".jsx", ".tsx":
			mimeType = "application/javascript"
		default:
			mimeType = "text/plain"
		}

		resources = append(resources, Resource{
			URI:         "file://" + filepath.ToSlash(filepath.Clean(path)),
			Name:        baseName,
			Description: description,
			MIMEType:    mimeType,
		})
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("walk error: %w", err)
	}
	return resources, nil
}

// =============================================================================
// Core Server Logic
// =============================================================================

func main() {
	defer func() {
		if r := recover(); r != nil {
			fmt.Fprintf(os.Stderr, "fatal panic recovered: %v\n", r)
			main() // restart loop (simple but effective)
		}
	}()
	scanner := bufio.NewScanner(os.Stdin)
	buf := make([]byte, 10*1024*1024)
	scanner.Buffer(buf, cap(buf))

	// Register resources at startup so they're available on every connection.
	resources, err := registerDirectoryResources("aidocs", "AI documentation and reference materials")
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: could not register aidocs resources: %v\n", err)
	}
	handleLine := func(line []byte, resources []Resource) bool {
		var req Request
		if err := json.Unmarshal(line, &req); err != nil {
			writeResponse(req.ID, nil, &ErrorResponse{Code: -32700, Message: "Parse error"})
			return true
		}
		switch req.Method {
		case "initialize":
			handleInitialize(&req)
		case "tools/list":
			handleToolsList(&req)
		case "resources/list":
			writeResponse(req.ID, &ResponseResult{Resources: resources}, nil)
		case "resources/read":
			var p struct {
				URI string `json:"uri"`
			}
			if err := json.Unmarshal(req.Params, &p); err != nil || p.URI == "" {
				writeResponse(req.ID, nil, &ErrorResponse{Code: -32602, Message: "Missing or invalid 'uri' parameter"})
				return true
			}
			content, _, err := readResource(p.URI)
			if err != nil {
				writeResponse(req.ID, nil, &ErrorResponse{Code: -32603, Message: err.Error()})
				return true
			}
			writeResponse(req.ID, &ResponseResult{Content: []ContentBlock{{Type: "text", Text: content}}}, nil)
		case "tools/call":
			handleToolCall(&req)
		default:
			writeResponse(req.ID, nil, &ErrorResponse{Code: -32601, Message: "Method not found"})
		}
		return true
	}
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		func() {
			defer func() {
				if r := recover(); r != nil {
					fmt.Fprintf(os.Stderr, "request panic: %v\n", r)
				}
			}()
			handleLine(line, resources)
		}()
	}

	if err := scanner.Err(); err != nil {
		fmt.Fprintf(os.Stderr, "stdin error: %v\n", err)
		os.Exit(1)
	}
}

func writeResponse(id interface{}, result *ResponseResult, err *ErrorResponse) {
	resp := Response{JSONRPC: "2.0", ID: id}
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
		Capabilities: map[string]interface{}{
			"tools":     struct{}{},
			"resources": struct{}{},
		},
		ServerInfo: map[string]string{"name": "mcp-fetch-server", "version": "1.0.0"},
	}, nil)
}

func handleToolsList(req *Request) {
	tools := []Tool{
		{
			Name:        "fetch_url",
			Description: "Fetches a URL over HTTP and converts its HTML content into markdown text.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"url":{"type":"string","description":"The URL to fetch and convert into markdown text"}},"required":["url"]}`),
		},
		{
			Name:        "list_directory",
			Description: "Lists the files and directories in a given path. Useful for understanding project structure.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string","description":"The directory path to list. Use '.' for the current directory."}},"required":["path"]}`),
		},
		{
			Name:        "read_file",
			Description: "Reads the content of a file from the local filesystem.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string","description":"The path to the file you want to read."}},"required":["path"]}`),
		},
		{
			Name:        "create_new_file",
			Description: "Creates a file at the given path with the provided content. If file exist and you can set overwrite to true to override.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string","description":"The path where the new file should be created."},"content":{"type":"string","description":"The text content to write into the file."},"overwrite":{"type":"boolean","default":false}},"required":["path","content"]}`),
		},
		{
			Name:        "run_terminal_command",
			Description: "Runs a shell command and returns its stdout and stderr.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"command":{"type":"string"},"working_dir":{"type":"string"},"confirmed":{"type":"boolean","default":false}},"required":["command"]}`),
		},
		{
			Name:        "file_glob_search",
			Description: "Searches for files matching a glob pattern under a root directory and returns their paths.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"root":{"type":"string"},"pattern":{"type":"string"},"max_results":{"type":"integer","default":100}},"required":["root","pattern"]}`),
		},
		{
			Name:        "find_replace_in_file",
			Description: "Performs a single find-and-replace operation in a file. Support golang regex in the find string if use_regex enabled",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"},"find":{"type":"string"},"replace":{"type":"string"},"use_regex":{"type":"boolean","default":false},"replace_all":{"type":"boolean","default":false}},"required":["path","find","replace"]}`),
		},
		{
			Name:        "http_request",
			Description: "Sends an HTTP request to an external URL with support for custom methods, headers, and request body. Useful for API calls, data retrieval, and performing external operations. Pass headers via the 'headers' parameter for authentication.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"method":{"type":"string","description":"HTTP method (GET, POST, PUT, DELETE, PATCH, HEAD, OPTIONS)","url":{"type":"string","description":"The URL to send the request to"},"headers":{"type":"object","description":"Optional HTTP headers (e.g., Authorization: Bearer XXX, )"},"body":{"type":"string","description":"Optional request body (string or JSON)"}}},"required":["method","url"]}`),
		},
	}

	writeResponse(req.ID, &ResponseResult{Tools: tools}, nil)
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
	case "fetch_url":
		var p struct {
			URL string `json:"url"`
		}
		if err := json.Unmarshal(args.Arguments, &p); err != nil || strings.TrimSpace(p.URL) == "" {
			writeResponse(req.ID, nil, &ErrorResponse{Code: -32602, Message: "Missing or invalid 'url' parameter"})
			return
		}
		md, err := fetchAndConvert(p.URL)
		if err != nil {
			writeResponse(req.ID, nil, &ErrorResponse{Code: -32603, Message: err.Error()})
			return
		}
		writeResponse(req.ID, &ResponseResult{Content: []ContentBlock{{Type: "text", Text: md}}}, nil)

	case "list_directory":
		var p struct {
			Path string `json:"path"`
		}
		if err := json.Unmarshal(args.Arguments, &p); err != nil || p.Path == "" {
			writeResponse(req.ID, nil, &ErrorResponse{Code: -32602, Message: "Missing or invalid 'path' parameter"})
			return
		}
		out, err := listDirectory(p.Path)
		if err != nil {
			writeResponse(req.ID, nil, &ErrorResponse{Code: -32603, Message: err.Error()})
			return
		}
		writeResponse(req.ID, &ResponseResult{Content: []ContentBlock{{Type: "text", Text: out}}}, nil)

	case "read_file":
		var p struct {
			Path string `json:"path"`
		}
		if err := json.Unmarshal(args.Arguments, &p); err != nil || p.Path == "" {
			writeResponse(req.ID, nil, &ErrorResponse{Code: -32602, Message: "Missing or invalid 'path' parameter"})
			return
		}
		content, err := readFileContent(p.Path)
		if err != nil {
			writeResponse(req.ID, nil, &ErrorResponse{Code: -32603, Message: err.Error()})
			return
		}
		writeResponse(req.ID, &ResponseResult{Content: []ContentBlock{{Type: "text", Text: content}}}, nil)

	case "create_new_file":
		var p struct {
			Path      string `json:"path"`
			Content   string `json:"content"`
			Overwrite bool   `json:"overwrite"`
		}
		if err := json.Unmarshal(args.Arguments, &p); err != nil || p.Path == "" {
			writeResponse(req.ID, nil, &ErrorResponse{Code: -32602, Message: "Missing or invalid parameters for create_new_file"})
			return
		}
		result, err := createNewFile(p.Path, p.Content, p.Overwrite)
		if err != nil {
			writeResponse(req.ID, nil, &ErrorResponse{Code: -32603, Message: err.Error()})
			return
		}
		writeResponse(req.ID, &ResponseResult{Content: []ContentBlock{{Type: "text", Text: result}}}, nil)

	case "run_terminal_command":
		var p struct {
			Command    string `json:"command"`
			WorkingDir string `json:"working_dir"`
			Confirmed  bool   `json:"confirmed"`
		}
		if err := json.Unmarshal(args.Arguments, &p); err != nil || strings.TrimSpace(p.Command) == "" {
			writeResponse(req.ID, nil, &ErrorResponse{Code: -32602, Message: "Missing or invalid 'command' parameter"})
			return
		}
		result, err := runTerminalCommand(p.Command, p.WorkingDir, p.Confirmed)
		if err != nil {
			writeResponse(req.ID, nil, &ErrorResponse{Code: -32603, Message: err.Error()})
			return
		}
		writeResponse(req.ID, &ResponseResult{Content: []ContentBlock{{Type: "text", Text: result}}}, nil)

	case "file_glob_search":
		var p struct {
			Root       string `json:"root"`
			Pattern    string `json:"pattern"`
			MaxResults int    `json:"max_results"`
		}
		if err := json.Unmarshal(args.Arguments, &p); err != nil || p.Root == "" || p.Pattern == "" {
			writeResponse(req.ID, nil, &ErrorResponse{Code: -32602, Message: "Missing or invalid parameters for file_glob_search"})
			return
		}
		if p.MaxResults <= 0 {
			p.MaxResults = 100
		}
		result, err := fileGlobSearch(p.Root, p.Pattern, p.MaxResults)
		if err != nil {
			writeResponse(req.ID, nil, &ErrorResponse{Code: -32603, Message: err.Error()})
			return
		}
		writeResponse(req.ID, &ResponseResult{Content: []ContentBlock{{Type: "text", Text: result}}}, nil)

	case "find_replace_in_file":
		var p struct {
			Path       string `json:"path"`
			Find       string `json:"find"`
			Replace    string `json:"replace"`
			UseRegex   bool   `json:"use_regex"`
			ReplaceAll bool   `json:"replace_all"`
		}
		if err := json.Unmarshal(args.Arguments, &p); err != nil || p.Path == "" || p.Find == "" {
			writeResponse(req.ID, nil, &ErrorResponse{Code: -32602, Message: "Missing or invalid parameters for find_replace_in_file"})
			return
		}
		result, err := findReplaceInFile(p.Path, p.Find, p.Replace, p.UseRegex, p.ReplaceAll)
		if err != nil {
			writeResponse(req.ID, nil, &ErrorResponse{Code: -32603, Message: err.Error()})
			return
		}
		writeResponse(req.ID, &ResponseResult{Content: []ContentBlock{{Type: "text", Text: result}}}, nil)

	case "http_request":
		var p struct {
			Method  string            `json:"method"`
			URL     string            `json:"url"`
			Body    string            `json:"body,omitempty"`
			Headers map[string]string `json:"headers,omitempty"`
		}
		if err := json.Unmarshal(args.Arguments, &p); err != nil || p.URL == "" || p.Method == "" {
			writeResponse(req.ID, nil, &ErrorResponse{Code: -32602, Message: "Missing or invalid 'method' or 'url' parameter"})
			return
		}
		result, err := makeHTTPRequest(p.Method, p.URL, p.Body, p.Headers)
		if err != nil {
			writeResponse(req.ID, nil, &ErrorResponse{Code: -32603, Message: err.Error()})
			return
		}
		writeResponse(req.ID, &ResponseResult{Content: []ContentBlock{{Type: "text", Text: result}}}, nil)

	default:
		writeResponse(req.ID, nil, &ErrorResponse{Code: -32601, Message: fmt.Sprintf("Tool '%s' not found", args.Name)})
	}
}

// =============================================================================
// Tool Implementations
// =============================================================================

// =============================================================================
// HTTP Request Tool Implementation
// =============================================================================

func makeHTTPRequest(method, url, body string, headers map[string]string) (string, error) {
	client := &http.Client{Timeout: 15 * time.Second}

	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}

	req, err := http.NewRequest(method, url, reader)
	if err != nil {
		return "", fmt.Errorf("invalid request: %w", err)
	}

	for k, v := range headers {
		req.Header.Set(k, v)
	}

	// Default Content-Type for non-GET requests
	if req.Method != "GET" && req.Method != "HEAD" && req.Header.Get("Content-Type") == "" {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("failed to read response body: %w", err)
	}

	sb := strings.Builder{}
	sb.WriteString(fmt.Sprintf("HTTP %s %s → %d %s\n", req.Method, url, resp.StatusCode, resp.Status))
	sb.WriteString(strings.Repeat("-", 50) + "\n")
	sb.WriteString("--- Response ---\n")

	contentType := resp.Header.Get("Content-Type")
	if strings.Contains(contentType, "json") {
		sb.WriteString("Content-Type: application/json\n\n")
		var prettyJSON interface{}
		if err := json.Unmarshal(respBody, &prettyJSON); err == nil {
			encoded, _ := json.MarshalIndent(prettyJSON, "", "  ")
			sb.Write(encoded)
			sb.WriteString("\n")
		} else {
			sb.Write(respBody)
		}
	} else {
		sb.WriteString(fmt.Sprintf("Content-Type: %s\n", contentType))
		sb.Write(respBody)
	}

	if len(respBody) > 1024 {
		sb.WriteString(fmt.Sprintf("\n\n(response truncated, %d bytes total)", len(respBody)))
	}

	return sb.String(), nil
}

func readFileContent(targetPath string) (string, error) {
	cleanPath := filepath.Clean(targetPath)
	if strings.Contains(cleanPath, "..") {
		return "", fmt.Errorf("unsafe path: path traversal detected")
	}
	content, err := os.ReadFile(cleanPath)
	if err != nil {
		return "", fmt.Errorf("failed to read file: %w", err)
	}
	return string(content), nil
}

func listDirectory(targetPath string) (string, error) {
	cleanPath := filepath.Clean(targetPath)
	if strings.Contains(cleanPath, "..") {
		return "", fmt.Errorf("unsafe path: path traversal detected")
	}
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

func createNewFile(path, content string, overwrite bool) (string, error) {
	cleanPath := filepath.Clean(path)
	if err := os.MkdirAll(filepath.Dir(cleanPath), 0755); err != nil {
		return "", fmt.Errorf("failed to create parent directories: %w", err)
	}
	if !overwrite {
		if _, err := os.Stat(cleanPath); err == nil {
			return "", fmt.Errorf("file already exists: %s (set overwrite=true to replace it)", cleanPath)
		}
	}
	f, err := os.OpenFile(cleanPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0644)
	if err != nil {
		return "", fmt.Errorf("failed to create file: %w", err)
	}
	defer f.Close()
	if _, err := f.WriteString(content); err != nil {
		return "", fmt.Errorf("failed to write content: %w", err)
	}
	return fmt.Sprintf("File created successfully: %s (%d bytes)", cleanPath, len(content)), nil
}

func runTerminalCommand(command, workingDir string, confirmed bool) (string, error) {
	if !confirmed {
		return fmt.Sprintf(
			"⚠️  Confirmation required\n\nThe following command has NOT been executed yet:\n\n  %s\n\nTo run it, call run_terminal_command again with confirmed=true.", command), nil
	}
	cmd := exec.Command("sh", "-c", command)
	if workingDir != "" {
		cmd.Dir = filepath.Clean(workingDir)
	}
	var stdout, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
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

func fileGlobSearch(root, pattern string, maxResults int) (string, error) {
	cleanRoot := filepath.Clean(root)
	recursive, namePattern := false, pattern
	if strings.HasPrefix(pattern, "**/") {
		recursive = true
		namePattern = strings.TrimPrefix(pattern, "**/")
	}
	var matches []string
	err := filepath.WalkDir(cleanRoot, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		matched, matchErr := false, error(nil)
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

// =============================================================================
// Resource read handler (called on resources/read)
// =============================================================================

func readResource(uri string) (string, string, error) {
	path := uri
	// Strip the file:// prefix if present
	path = strings.TrimPrefix(path, "file://")
	path = filepath.Clean(path)
	// Reject path traversal attempts
	if strings.Contains(path, "..") || filepath.IsAbs(path) && !strings.HasPrefix(path, "/") {
		return "", "", fmt.Errorf("unsafe URI: path traversal detected")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", "", fmt.Errorf("failed to read resource %s: %w", uri, err)
	}
	// Determine MIME type from extension
	ext := strings.ToLower(filepath.Ext(path))
	var mimeType string
	switch ext {
	case ".md":
		mimeType = "text/markdown"
	case ".txt":
		mimeType = "text/plain"
	case ".json", ".yaml", ".yml":
		mimeType = "application/json"
	case ".xml":
		mimeType = "application/xml"
	case ".html", ".htm":
		mimeType = "text/html"
	case ".css":
		mimeType = "text/css"
	case ".js", ".ts", ".jsx", ".tsx":
		mimeType = "application/javascript"
	default:
		mimeType = "text/plain"
	}
	return string(data), mimeType, nil
}
