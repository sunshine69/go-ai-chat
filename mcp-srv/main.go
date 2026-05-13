package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"

	mcp "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

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

	pwProxy, err := NewPlaywrightProxy()
	if err != nil {
		log.Printf("Warning: Playwright MCP unavailable: %v", err)
	} else {
		registerPlaywrightTools(s, pwProxy)
	}

	pg := NewPostgresManager()
	registerPostgresTools(s, pg)

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
		mcp.WithDescription("Runs a shell command and returns its stdout and stderr. you should try other tools first and only use this as last resort. If the command does not return it will block you."),
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

	// Define flags
	flag.StringVar(&cfg.transport, "t", cfg.transport, "Transport: \"stdio\" (default), \"sse\", or \"streamable\"")
	flag.StringVar(&cfg.host, "H", cfg.host, "Host to listen on (shorthand)")
	flag.IntVar(&cfg.port, "p", cfg.port, "Port to listen on (shorthand)")
	flag.StringVar(&cfg.basePath, "base-path", cfg.basePath, "URL base path prefix")

	flag.Usage = printUsage
	flag.Parse()
	return cfg
}

func printUsage() {
	fmt.Fprintf(os.Stderr, `Usage: mcp-server [options]

Options:
  -t   		  Transport: "stdio" (default), "streamable"
  -H   		  Host to listen on (default: 0.0.0.0)
  -p   		  Port to listen on (default: 8080)
  -base-path  URL base path prefix (default: "")
  -h    	  Show this help

Examples:
  # stdio (default — Claude Desktop / local MCP clients)
  mcp-server

  # Streamable HTTP transport (newer MCP clients, llama-server web UI)
  mcp-server -t streamable -p 8081
    POST /mcp      — single endpoint for all JSON-RPC

  # Both transports on different ports (serve all clients simultaneously):
  mcp-server -t streamable -p 8081 &
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
