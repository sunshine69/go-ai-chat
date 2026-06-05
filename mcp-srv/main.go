package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"runtime"
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
	toolSet   string // extra tools to load, comma sep
}

var (
	defaultAllowCmd  string
	defaultAllowPath string
)

func init() {
	// The allow patterns act as a whitelist — anything not matching is denied,
	// so a block pattern on top would be redundant. Block patterns are left empty
	// by default; users can set BLOCKED_TERM_CMD_PTN / BLOCKED_PATH_PTN themselves
	// if they want a denylist-only approach (i.e. no allow pattern set).
	//
	// NOTE: bare shells (bash/sh/cmd/powershell) are intentionally omitted.
	// Allowing them lets an agent run anything via `bash -c "..."`, bypassing
	// this check entirely. The MCP server's own file/search tools cover most
	// use cases that would otherwise need shell piping. Add them deliberately
	// if you accept that trade-off.

	// ---------------------------------------------------------------------------
	// Shared — identical on every platform. Each group is a string fragment that
	// gets concatenated into the final regex alternation.
	// ---------------------------------------------------------------------------
	sharedCmds := `` +
		// --- Go toolchain ---
		`go|gobind|gofmt|gomobile|govet|golangci-lint|staticcheck|` +
		// --- Rust ---
		`cargo|rustc|rustfmt|clippy|rustup|` +
		// --- Java / JVM ---
		`java|javac|javadoc|jar|mvn|mvnw|gradle|gradlew|ant|kotlin|kotlinc|scala|scalac|sbt|` +
		// --- .NET (cross-platform subset) ---
		`dotnet|mono|paket|fsi|` +
		// --- Python ---
		`python|python3|pip|pip3|uv|pipenv|poetry|pytest|pylint|flake8|mypy|ruff|black|isort|` +
		// --- Node / JS / TS ---
		`node|npm|npx|yarn|pnpm|bun|ng|tsc|eslint|prettier|jest|mocha|vitest|webpack|vite|esbuild|` +
		// --- Ruby ---
		`ruby|gem|bundle|rake|rspec|rubocop|` +
		// --- PHP ---
		`php|composer|phpunit|phpcs|phpstan|` +
		// --- Mobile ---
		`adb|fastboot|flutter|dart|` +
		// --- Version control ---
		`git|gh|gitlab|hg|svn|git-lfs|pre-commit|` +
		// --- Containers ---
		// `docker|podman|` +
		// --- Kubernetes ---
		`kubectl|helm|helmfile|kustomize|skaffold|argocd|flux|istioctl|linkerd|` +
		`kind|minikube|kubectx|kubens|stern|k9s|` +
		// --- IaC ---
		`terraform|terragrunt|tofu|pulumi|packer|ansible|ansible-playbook|ansible-vault|ansible-galaxy|vagrant|` +
		// --- AWS ---
		`aws|aws-vault|sam|cdk|amplify|copilot|eksctl|` +
		// --- GCP ---
		`gcloud|gsutil|bq|firebase|` +
		// --- Azure ---
		`az|azd|azcopy|func|bicep|` +
		// --- HashiCorp (non-terraform) ---
		`vault|consul|nomad|boundary|` +
		// --- Serverless / modern platforms ---
		`serverless|sls|fly|vercel|netlify|wrangler|supabase|` +
		// --- Database CLIs ---
		`psql|mysql|sqlite3|mongosh|redis-cli|` +
		// --- Secrets / security ---
		`sops|op|chamber|age|` +
		// --- Archive / transfer ---
		`tar|zip|unzip|7z|curl|wget|` +
		// --- Misc dev tools ---
		`jq|yq|`

	// ---------------------------------------------------------------------------
	// Platform-specific additions — things that only exist (or only make sense)
	// on that OS, then combined with sharedCmds into the final pattern.
	// ---------------------------------------------------------------------------
	switch runtime.GOOS {
	case "windows":
		windowsCmds :=
			// --- C / C++ (MSVC + MinGW) ---
			`gcc|g\+\+|clang|clang\+\+|cl|link|make|cmake|ninja|msbuild|nmake|` +
				// --- .NET (Windows-only tools) ---
				`nuget|msbuild|vstest\.console|signtool|csc|vbc|` +
				// --- Windows shell builtins ---
				`where|type|dir|echo|set|copy|move|del|mkdir|rmdir|xcopy|robocopy|attrib|icacls|` +
				// --- WSL ---
				`wsl|`

		defaultAllowCmd = `^(` + sharedCmds + windowsCmds + `)[\s]+.*$`

		// Allow: %TEMP%/%TMP%/%USERPROFILE% literals, relative paths (no drive
		// letter or leading backslash), and absolute paths under Users\, Temp\,
		// Windows\Temp\. (?i) because Windows paths are case-insensitive.
		defaultAllowPath = `(?i)(^(%TEMP%|%TMP%|%USERPROFILE%)[/\\]|^[A-Za-z]:[/\\](Users|Temp|tmp|Windows[/\\]Temp)[/\\]|^[^\\/:*?"<>|][^:*?"<>|]*$)`

	default: // linux, darwin, and everything else
		unixCmds :=
			// --- C / C++ ---
			`gcc|g\+\+|clang|clang\+\+|make|cmake|ninja|m4|bison|flex|` +
				// --- Scripting ---
				`perl|lua|` +
				// --- macOS ---
				`xcodebuild|xcrun|brew|open|pbcopy|` +
				// --- File / text utils ---
				`cat|grep|find|sed|awk|head|tail|wc|diff|patch|sort|uniq|xargs|ls|cp|mv|rm|` +
				`chmod|chown|touch|tee|cut|tr|file|stat|du|df|ln|realpath|dirname|basename|` +
				// --- Network ---
				`ssh|scp|rsync|nc|` +
				// --- Archive / compression (unix-only formats) ---
				`gzip|gunzip|bzip2|xz|zstd|lzma|` +
				// --- Linux package managers ---
				`apt|apt-get|apt-cache|yum|dnf|apk|pacman|snap|` +
				// --- Unix misc ---
				`echo|env|which|date|pwd|uname|tput|xdg-open|xclip|` +
				// --- gradlew wrapper (unix executable) ---
				`\.\/gradlew|`

		defaultAllowCmd = `^(` + sharedCmds + unixCmds + `)[\s]+.*$`

		// Allow /tmp/, /var/tmp/, or any relative path (no leading /).
		// The old pattern `(\/tmp|[^\/])[^\s]*$` had a bug: [^\/] matched any
		// single non-slash char, so `/etc/shadow` passed because `shadow` starts
		// with `s`. Anchoring explicitly closes that gap.
		defaultAllowPath = `(^/tmp/|^/var/tmp/|^[^/])[^\s]*$`
	}
}

func parseArgs() config {
	cfg := config{
		transport: "stdio",
		host:      "0.0.0.0",
		port:      8080,
		basePath:  "",
	}

	flag.StringVar(&cfg.transport, "t", cfg.transport, `Transport: "stdio" (default), "sse", or "streamable"`)
	flag.StringVar(&cfg.host, "H", cfg.host, "Host to listen on")
	flag.IntVar(&cfg.port, "p", cfg.port, "Port to listen on")
	flag.StringVar(&cfg.basePath, "base-path", cfg.basePath, "URL base path prefix")
	flag.StringVar(&cfg.toolSet, "tools", "", "Extra tools to load in addition to base tools. Comma-separated. Possible values: postgres, octo, browser, godoc, all")

	flag.Usage = printUsage
	flag.Parse()
	return cfg
}

func printUsage() {
	fmt.Fprintf(os.Stderr, `Usage: mcp-server [options]

Options:
  -t            Transport: "stdio" (default), "streamable"
  -H            Host to listen on (default: 0.0.0.0)
  -p            Port to listen on (default: 8080)
  -base-path    URL base path prefix (default: "")
  -tools        [all,postgres,octo,browser,godoc] Comma-separated list of extra
                tool sets to load in addition to the base tools. Default: empty.
  -h            Show this help

Examples:
  # stdio (default — Claude Desktop / local MCP clients)
  mcp-server

  # Streamable HTTP transport (newer MCP clients, llama-server web UI)
  mcp-server -t streamable -p 8081
    POST /mcp   — single endpoint for all JSON-RPC

Environment variables (all accept Go regex patterns):
  ALLOWED_TERM_CMD_PTN   Whitelist for run_terminal_command. Anything not matching
                         is denied. Default (%s):
                         %s

  BLOCKED_TERM_CMD_PTN   Optional denylist (useful when no allow pattern is set).
                         Default: ""

  ALLOWED_PATH_PTN       Whitelist for all file/directory operations. Anything not
                         matching is denied. Default (%s):
                         %s

  BLOCKED_PATH_PTN       Optional denylist (useful when no allow pattern is set).
                         Default: ""

`, runtime.GOOS, defaultAllowCmd, runtime.GOOS, defaultAllowPath)
}

// buildServer registers all tool sets onto an MCPServer instance.
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

		// Wrap with CORS middleware so browser-based clients (e.g. llama-server
		// web UI) can connect without a proxy.
		corsHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Access-Control-Allow-Origin", "*")
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS, DELETE")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, Accept, Mcp-Session-Id, Last-Event-ID, Mcp-Protocol-Version")
			w.Header().Set("Access-Control-Expose-Headers", "Mcp-Session-Id")

			// Respond to preflight and stop — if we fall through, the inner handler
			// writes its own response and the headers are already locked.
			if r.Method == http.MethodOptions {
				w.WriteHeader(http.StatusNoContent)
				return
			}

			streamServer.ServeHTTP(w, r)
		})

		log.Printf("Starting MCP server (Streamable HTTP + CORS) on http://%s%s", addr, endpoint)
		log.Printf("  Endpoint: POST http://%s%s", addr, endpoint)
		if err := http.ListenAndServe(addr, corsHandler); err != nil {
			log.Fatalf("Streamable HTTP server error: %v", err)
		}

	default:
		fmt.Fprintf(os.Stderr, "Unknown transport %q — valid values: \"stdio\", \"streamable\"\n", cfg.transport)
		printUsage()
		os.Exit(1)
	}
}
