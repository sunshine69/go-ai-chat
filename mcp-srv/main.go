package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	html2md "github.com/JohannesKaufmann/html-to-markdown"
	mcp "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// =============================================================================
// Tool Implementations
// =============================================================================

func readFileContent(targetPath string) (string, error) {
	cleanPath := filepath.Clean(targetPath)
	content, err := os.ReadFile(cleanPath)
	if err != nil {
		return "", fmt.Errorf("failed to read file: %w", err)
	}
	return string(content), nil
}

func listDirectory(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
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

func createNewFile(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
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

func runTerminalCommand(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := request.GetArguments()
	command := ""
	if c, ok := args["command"]; ok {
		command = fmt.Sprintf("%v", c)
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

func fileGlobSearch(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
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

func findReplaceInFile(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
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
			return mcp.NewToolResultError(fmt.Sprintf("invalid regex pattern: %w", compileErr)), nil
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
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("HTTP error: %d %s\nResponse body: %s", resp.StatusCode, resp.Status, string(body))
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("read body failed: %w", err)
	}

	converter := html2md.NewConverter("", true, nil)
	md, err := converter.ConvertString(string(body))
	if err != nil {
		return "", fmt.Errorf("html to md conversion failed: %w", err)
	}

	return strings.TrimSpace(md), nil
}

func fetchUrl(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
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
	return mcp.NewToolResultText(markdownText), nil
}

func httpRequest(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
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

	return mcp.NewToolResultText(fmt.Sprintf("Status: %d\nBody: %s", resp.StatusCode, string(respBody))), nil
}

// =============================================================================
// Server builder — registers all tools onto an MCPServer instance
// =============================================================================

func buildServer() *server.MCPServer {
	s := server.NewMCPServer(
		"mcp-fetch-server",
		"1.0.0",
		server.WithToolCapabilities(true),
		server.WithResourceCapabilities(true, true),
	)

	s.AddTool(mcp.NewTool("fetch_url",
		mcp.WithDescription("Fetches a URL over HTTP and converts its HTML content into markdown text."),
		mcp.WithString("url", mcp.Required(), mcp.Description("The URL to fetch and convert into markdown text")),
	), fetchUrl)

	s.AddTool(mcp.NewTool("list_directory",
		mcp.WithDescription("Lists the files and directories in a given path. Useful for understanding project structure."),
		mcp.WithString("path", mcp.Required(), mcp.Description("The directory path to list. Use '.' for the current directory.")),
	), listDirectory)

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
		mcp.WithDescription("Creates a new file at the given path with the provided content. Fails if the file already exists unless overwrite is set to true."),
		mcp.WithString("path", mcp.Required(), mcp.Description("The path where the new file should be created.")),
		mcp.WithString("content", mcp.Required(), mcp.Description("The text content to write into the file.")),
		mcp.WithBoolean("overwrite", mcp.DefaultBool(false), mcp.Description("If true, overwrite the file if it already exists. Defaults to false.")),
	), createNewFile)

	s.AddTool(mcp.NewTool("run_terminal_command",
		mcp.WithDescription("Runs a shell command and returns its stdout and stderr."),
		mcp.WithString("command", mcp.Required(), mcp.Description("The shell command to execute.")),
		mcp.WithString("working_dir", mcp.Description("Optional working directory for the command. Defaults to the server's current directory.")),
		mcp.WithBoolean("confirmed", mcp.DefaultBool(false), mcp.Description("Must be explicitly set to true to actually run the command. When false or omitted a confirmation message is returned and nothing is executed.")),
	), runTerminalCommand)

	s.AddTool(mcp.NewTool("file_glob_search",
		mcp.WithDescription("Searches for files matching a glob pattern under a root directory and returns their paths."),
		mcp.WithString("root", mcp.Required(), mcp.Description("The root directory to search within.")),
		mcp.WithString("pattern", mcp.Required(), mcp.Description("Glob pattern to match filenames (e.g. '*.go', '**/*.json').")),
		mcp.WithInteger("max_results", mcp.Description("Maximum number of results to return.")),
	), fileGlobSearch)

	s.AddTool(mcp.NewTool("find_replace_in_file",
		mcp.WithDescription("Performs a single find-and-replace operation in a file. Replaces the first (or all) occurrence(s) of a literal string or regex pattern with a replacement string."),
		mcp.WithString("path", mcp.Required(), mcp.Description("Path to the file to modify.")),
		mcp.WithString("find", mcp.Required(), mcp.Description("The string or regex pattern to search for.")),
		mcp.WithString("replace", mcp.Required(), mcp.Description("The replacement string. Supports $1, $2 … back-references when use_regex is true.")),
		mcp.WithBoolean("use_regex", mcp.DefaultBool(false), mcp.Description("Treat 'find' as a regular expression.")),
		mcp.WithBoolean("replace_all", mcp.DefaultBool(false), mcp.Description("Replace all occurrences instead of just the first.")),
	), findReplaceInFile)

	s.AddTool(mcp.NewTool("http_request",
		mcp.WithDescription("Make HTTP requests to external APIs"),
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
	), httpRequest)

	return s
}

// =============================================================================
// CLI flag parsing
// =============================================================================

type config struct {
	transport string // "stdio" | "sse" | "streamable"
	host      string
	port      int
	basePath  string
}

func parseArgs() config {
	cfg := config{
		transport: "stdio",
		host:      "0.0.0.0",
		port:      8080,
		basePath:  "",
	}

	args := os.Args[1:]
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--transport", "-t":
			if i+1 < len(args) {
				i++
				cfg.transport = args[i]
			}
		case "--host", "-H":
			if i+1 < len(args) {
				i++
				cfg.host = args[i]
			}
		case "--port", "-p":
			if i+1 < len(args) {
				i++
				fmt.Sscanf(args[i], "%d", &cfg.port)
			}
		case "--base-path":
			if i+1 < len(args) {
				i++
				cfg.basePath = args[i]
			}
		case "--help", "-h":
			printUsage()
			os.Exit(0)
		}
	}
	return cfg
}

func printUsage() {
	fmt.Fprintf(os.Stderr, `Usage: mcp-fetch-server [options]

Options:
  --transport, -t   Transport: "stdio" (default), "sse", or "streamable"
  --host,      -H   Host to listen on (default: 0.0.0.0)
  --port,      -p   Port to listen on (default: 8080)
  --base-path       URL base path prefix (default: "")
  --help,      -h   Show this help

Examples:
  # stdio (default — Claude Desktop / local MCP clients)
  mcp-fetch-server

  # Legacy SSE transport (older MCP clients, aig)
  mcp-fetch-server --transport sse --port 8080
    GET  /sse      — SSE event stream
    POST /message  — JSON-RPC messages

  # Streamable HTTP transport (newer MCP clients, llama-server web UI)
  mcp-fetch-server --transport streamable --port 8081
    POST /mcp      — single endpoint for all JSON-RPC

  # Both transports on different ports (serve all clients simultaneously):
  mcp-fetch-server --transport sse        --port 8080 &
  mcp-fetch-server --transport streamable --port 8081 &
`)
}

// =============================================================================
// Main
// =============================================================================

func main() {
	cfg := parseArgs()

	s := buildServer()

	switch cfg.transport {
	case "stdio":
		log.Println("Starting MCP server (stdio transport)")
		if err := server.ServeStdio(s); err != nil {
			log.Fatalf("Server error: %v", err)
		}
	case "streamable":
		addr := fmt.Sprintf("%s:%d", cfg.host, cfg.port)
		endpoint := cfg.basePath + "/mcp"

		streamServer := server.NewStreamableHTTPServer(s,
			server.WithEndpointPath(endpoint),
		)

		// Wrap with CORS middleware so browser-based clients (llama-server web UI) can connect
		corsHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Set CORS headers on EVERY response, not just OPTIONS
			w.Header().Set("Access-Control-Allow-Origin", "*")
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS, DELETE")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, Accept, Mcp-Session-Id, Last-Event-ID")
			w.Header().Set("Access-Control-Expose-Headers", "Mcp-Session-Id")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, Accept, Mcp-Session-Id, Last-Event-ID, Mcp-Protocol-Version")

			// Must return before calling ServeHTTP — otherwise the inner handler
			// writes its own response and headers are locked
			if r.Method == http.MethodOptions {
				w.WriteHeader(http.StatusNoContent)
				return // <-- never reaches streamServer.ServeHTTP
			}

			streamServer.ServeHTTP(w, r)
		})

		log.Printf("Starting MCP server (Streamable HTTP + CORS) on http://%s%s", addr, endpoint)
		log.Printf("  Endpoint    : POST http://%s%s", addr, endpoint)
		if err := http.ListenAndServe(addr, corsHandler); err != nil {
			log.Fatalf("Streamable HTTP server error: %v", err)
		}

	default:
		fmt.Fprintf(os.Stderr, "Unknown transport %q — valid values: \"stdio\", \"sse\", \"streamable\"\n", cfg.transport)
		printUsage()
		os.Exit(1)
	}
}
