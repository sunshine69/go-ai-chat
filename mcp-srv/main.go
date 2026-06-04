package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"

	"github.com/mark3labs/mcp-go/server"
	u "github.com/sunshine69/golang-tools/utils"
)

// CLI flag parsing
type config struct {
	transport string // "stdio" | "sse" | "streamable"
	host      string
	port      int
	basePath  string
	toolSet   string // extra tools to laod, coma sep
}

var (
	defaultAllowCmd  = `^(go|gobind|gofmt|gomobile|gcc|g\+\+|make|cmake|m4|bison|jq|cat|docker|python3|perl|lua|pip3|pip|adb|dotnet|java|javac|npm|node|yarn|ng|npx|gradle|\.\/gradlew|)[\s]+.*$`
	defaultAllowPath = `(\/tmp|[^\/])[^\s]*$`
)

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
	flag.StringVar(&cfg.toolSet, "tools", "", "Extra tools name to load in addition to base. Coma sep string. Possible values: postgres,octo,browser or all")

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
  -tools      [all,postgres,octo,browser] List of tools name to load in addition to basic tools in coma separated list.
			  Default: empty (no load). all to load all of them.
  -h    	  Show this help

Examples:
  # stdio (default — Claude Desktop / local MCP clients)
  mcp-server

  # Streamable HTTP transport (newer MCP clients, llama-server web UI)
  mcp-server -t streamable -p 8081
    POST /mcp      — single endpoint for all JSON-RPC

  # Both transports on different ports (serve all clients simultaneously):
  mcp-server -t streamable -p 8081 &

  Environment vars you can set. They re all golang regex ptn.
	ALLOWED_TERM_CMD_PTN - the pattern to allow or block certain command used in the tool run_terminal_command. Default '%s'
	BLOCKED_TERM_CMD_PTN

	ALLOWED_PATH_PTN - the pattern to allow or block certain path used in all file/directory operations. Default '%s'
	BLOCKED_PATH_PTN

`, defaultAllowCmd, defaultAllowPath)
}

// Server builder — registers all tools onto an MCPServer instance
func buildServer(cfg config) *server.MCPServer {
	s := server.NewMCPServer(
		"mcp-fetch-server",
		"1.0.0",
		server.WithToolCapabilities(true),
		server.WithResourceCapabilities(true, true),
	)

	if strings.Contains(cfg.toolSet, "all") || strings.Contains(cfg.toolSet, "postgres") {
		pg := NewPostgresManager()
		registerPostgresTools(s, pg)
	}

	if strings.Contains(cfg.toolSet, "all") || strings.Contains(cfg.toolSet, "octo") {
		octo := NewOctopusManager()
		registerOctopusTools(s, octo)
	}

	if strings.Contains(cfg.toolSet, "all") || strings.Contains(cfg.toolSet, "browser") {
		pwProxy, err := NewPlaywrightProxy()
		if err != nil {
			log.Printf("Warning: Playwright MCP unavailable: %v", err)
		} else {
			registerPlaywrightTools(s, pwProxy)
		}
	}

	if strings.Contains(cfg.toolSet, "all") || strings.Contains(cfg.toolSet, "godoc") {
		pwProxy, err := NewPlaywrightProxy()
		if err != nil {
			log.Printf("Warning: Playwright MCP unavailable: %v", err)
		} else {
			registerPlaywrightTools(s, pwProxy)
			registerGoDocTools(s, pwProxy)
		}
	}
	baseTool := BaseToolManager{
		AllowedTerminalCommandPattern: u.Getenv("ALLOWED_TERM_CMD_PTN", defaultAllowCmd),
		BlockedTerminalCommandPattern: u.Getenv("BLOCKED_TERM_CMD_PTN", ""),
		AllowedPathPattern:            u.Getenv("ALLOWED_PATH_PTN", defaultAllowPath),
		BlockedPathPattern:            u.Getenv("BLOCKED_PATH_PTN", ""),
	}
	registerBaseTool(s, &baseTool)
	registerTextTools(s, &TextToolManager{})
	return s
}

func main() {
	cfg := parseArgs()
	s := buildServer(cfg)

	switch cfg.transport {
	case "stdio":
		log.Println("Starting MCP server (stdio transport)")
		if err := server.ServeStdio(s); err != nil {
			log.Fatalf("Server error: %v", err)
		}
	case "streamable", "streamablehttp":
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
