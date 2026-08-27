package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"regexp"
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
	workDir   string
	toolSet   string // extra tools to load, comma sep
}

var (
	defaultAllowCmd  string
	defaultAllowPath string
	ForbiddenString  = []string{` ~/. `, ` $HOME `, ` ${HOME} `}
	pathErrorMsg     string
	PathPtn          *regexp.Regexp
	unixFileTools    map[string]any = u.SliceToMap([]string{"cat", "find", "head", "ls", "cp", "mv", "rm", "chmod", "chown", "touch", "file", "stat", "ln", "realpath", "dirname", "basename", "cd"})
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
		// --- Go toolchain gorun is mine ---
		`go|gobind|gofmt|gomobile|govet|gorun|` +
		// --- Rust ---
		`cargo|rustc|rustfmt|rustup|` +
		// --- Java / JVM ---
		`java|javac|javadoc|jar|mvn|mvnw|gradle|gradlew|ant|kotlin|kotlinc|scala|scalac|sbt|keytool|openssl|` +
		// --- .NET (cross-platform subset) ---
		`dotnet|mono|paket|fsi|` +
		// --- Python ---
		`python3|pip3|uv|pipenv|poetry|pytest|pylint|flake8|` +
		// --- Node / JS / TS ---
		`node|npm|npx|yarn|pnpm|bun|ng|tsc|eslint|webpack|vite|esbuild|` +
		// --- Ruby ---
		// `ruby|gem|bundle|rake|rspec|rubocop|` +
		// --- PHP ---
		`php|composer|phpunit|phpcs|phpstan|` +
		// --- Mobile ---
		`adb|fastboot|flutter|dart|` +
		// --- Version control ---
		`git|gh|gitlab|git-lfs|pre-commit|` +
		// --- Containers ---
		// `docker|podman|` +
		// --- Kubernetes ---
		`kubectl|helm|kustomize|skaffold|argocd|flux|linkerd|` +
		`kind|minikube|kubens|k9s|` +
		// --- IaC ---
		`terraform|terragrunt|tofu|pulumi|packer|ansible|ansible-playbook|ansible-vault|ansible-galaxy|vagrant|` +
		// --- AWS ---
		`aws|aws-vault|sam|cdk|amplify|copilot|eksctl|` +
		// --- GCP ---
		// `gcloud|gsutil|bq|firebase|` +
		// --- Azure ---
		`az|azd|azcopy|func|bicep|` +
		// --- HashiCorp (non-terraform) ---
		`vault|consul|` +
		// --- Serverless / modern platforms ---
		// `serverless|sls|fly|vercel|netlify|wrangler|supabase|` +
		// --- Database CLIs ---
		`psql|mysql|sqlite3|mongosh|redis-cli|` +
		// --- Secrets / security ---
		// `sops|op|chamber|age|` +
		// --- Misc dev tools ---
		`jq|yq|perl|lua|`

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
			// We allow the space after, could be none which has a limitation, eg. if command 'cd' is not in the list but there is command cdk, then 'cd path' will be accepted. To solve it we might be throughly review each cmd and which one allow space which one not.
		defaultAllowCmd = `^(` + sharedCmds + windowsCmds + `)[\s]*.*$`

		// — regexp.MustCompile on that pattern panics at startup. This rewrite
		// gets the same "no .. anywhere" guarantee via segment-level alternation,
		// same technique as the unix pattern.
		//
		// Allows: relative paths (backslash or forward slash separators, one
		// "..\" step up), OR absolute paths under the literal env-var tokens
		// %TEMP%, %TMP%, %USERPROFILE% (agent should pass these unresolved —
		// the OS/shell expands them, we never see or need to know the real path).
		//
		// Deliberately does NOT allow bare drive-letter paths (C:\Users\...) or
		// UNC paths (\\server\share) — colon and leading backslash are excluded
		// from every segment, so both classes fail to match. If you want direct
		// drive-letter access, that's a real widening of the blast radius (any
		// path on that drive vs. a scoped temp/profile dir) and should be an
		// explicit, deliberate addition — same trade-off logic as the bare-shell
		// omission above.
		winSegment := `(?:[^./\\:*?"<>|][^\\/:*?"<>|]*|\.[^./\\:*?"<>|][^\\/:*?"<>|]*|\.)`
		sep := `[\\/]`
		winSegments := winSegment + `(?:` + sep + winSegment + `)*`
		winPathBody := winSegments + sep + `?`
		winRelative := `(?:(?:\.\.` + sep + `|\.` + sep + `)(?:` + winPathBody + `)?|` + winPathBody + `)`
		winAbs := `(?:%TEMP%|%TMP%|%USERPROFILE%)(?:` + sep + `(?:` + winPathBody + `)?)?`
		defaultAllowPath = `(?i)^(?:` + winRelative + `|` + winAbs + `)$`

		pathErrorMsg = `[ERROR] denied access for path: '%s'. ONLY RELATIVE PATH TO THE CURRENT DIR AND ONE LEVEL UPPER ARE ALLOWED. EXCEPTIONS ARE ABSOLUTE PATH START FROM TEMP DIR`
		PathPtn = regexp.MustCompile(`(?:[a-zA-Z]:\\|\\\\|\.\\|\.\.\\)[a-zA-Z0-9_\.\-\\]+`)

	default: // linux, darwin, and everything else
		unixCmds :=
			// --- C / C++ ---
			`gcc|g\+\+|clang|clang\+\+|make|cmake|ninja|m4|bison|flex|` +
				// --- Archive / transfer ---
				`tar|zip|unzip|7z|curl|wget|zstd|` +
				// --- macOS ---
				`xcodebuild|xcrun|brew|open|pbcopy|` +
				// --- File / text utils ---
				strings.Join(u.MapKeysToSlice(unixFileTools), "|") + "|sed|awk|cut|tr|du|df|grep|tail|wc|diff|patch|sort|uniq|xargs|" +
				// --- Network ---
				`ssh|scp|rsync|nc|` +
				// --- Archive / compression (unix-only formats) ---
				`gzip|gunzip|bzip2|xz|zstd|lzma|` +
				// --- Linux package managers ---
				// `apt|apt-get|apt-cache|yum|dnf|apk|pacman|snap|` +
				// --- Unix misc ---
				`echo|env|which|date|pwd|uname|tput|xdg-open|xclip|` +
				// --- gradlew wrapper (unix executable) ---
				`\.\/gradlew|`

		defaultAllowCmd = `^(` + sharedCmds + unixCmds + `)[\s]*.*$`

		// Relative-to-cwd (one "../" allowed), OR absolute under /tmp or /var/tmp.
		// Every path segment must not be exactly ".." — this closes traversal
		// both in the relative form (../../etc/passwd) and inside the /tmp
		// exception (/tmp/../etc/shadow). See detailed segment-matching notes
		// in the earlier version of this comment.
		pathSegment := `(?:[^./:\s][^/\s]*|\.[^./\s][^/\s]*|\.)`
		segments := pathSegment + `(?:/` + pathSegment + `)*`
		pathBody := segments + `/?` // fix: allow one optional trailing slash after the last segment
		relativePath := `(?:(?:\.\./|\./)(?:` + pathBody + `)?|` + pathBody + `)`
		tmpPath := `(?:/tmp|/var/tmp)(?:/(?:` + pathBody + `)?)?`
		defaultAllowPath = `^(?:` + relativePath + `|` + tmpPath + `)$`

		pathErrorMsg = `[ERROR] denied access for path: '%s'. ONLY RELATIVE PATH TO THE CURRENT DIR AND ONE LEVEL UPPER ARE ALLOWED. EXCEPTIONS ARE /tmp and /var/tmp. That is ./XXX ../XXX XXX /tmp, /var/tmp should work, BUT NOT / and ../../`
		PathPtn = regexp.MustCompile(`(?:^|\s)([\.\/][a-zA-Z0-9_\.\-\/]+)`)
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
	flag.StringVar(&cfg.workDir, "work-dir", ".", "Working dir")
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
	u.CheckErr(os.Chdir(cfg.workDir), "Can not chdir to work-dir "+cfg.workDir)
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

	if strings.Contains(cfg.toolSet, "all") || strings.Contains(cfg.toolSet, "svg") {
		RegisterSVGTools(s, NewSVGToolManager())
	}

	if strings.Contains(cfg.toolSet, "all") || strings.Contains(cfg.toolSet, "rustdoc") {
		registerRustDocTools(s)
	}

	if strings.Contains(cfg.toolSet, "all") || strings.Contains(cfg.toolSet, "skills") {
		registerSkillsTools(s, u.Must(NewSkillsProxy()))
	}

	println("[DEBUG] defaultAllowPath - ", defaultAllowPath)
	baseTool := BaseToolManager{ // Noticed very very strange behaviour of env var corruptions when using tmux
		AllowedTerminalCommandPattern: u.Getenv("ALLOWED_TERM_CMD_PTN", defaultAllowCmd),
		BlockedTerminalCommandPattern: u.Getenv("BLOCKED_TERM_CMD_PTN", ""),
		AllowedPathPattern:            defaultAllowPath,
		BlockedPathPattern:            u.Getenv("BLOCKED_PATH_PTN", ""),
	}
	println("[DEBUG] baseTool - ", u.JsonDump(baseTool, ""))
	// Default tools to load
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
