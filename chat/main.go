package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/joho/godotenv"
	"github.com/reeflective/readline"
	u "github.com/sunshine69/golang-tools/utils"
)

var pendingFileContent []ContentPart

// ---------------------------------------------------------------------------
// extractInlineFiles — returns []ContentPart for file content
// ---------------------------------------------------------------------------

func extractInlineFiles(text string) (cleanText string, fileParts []ContentPart) {
	words := strings.Fields(text)
	var kept []string

	for _, w := range words {
		if strings.HasPrefix(w, "file://") {
			path := strings.TrimPrefix(w, "file://")
			parts, err := processFile(path)
			if err != nil {
				fmt.Fprintf(os.Stderr, "⚠️  Could not load file %s: %v\n", path, err)
				kept = append(kept, w)
			} else {
				fmt.Fprintf(os.Stderr, "📎 Loaded inline file: %s\n", path)
				fileParts = append(fileParts, parts...)
			}
		} else {
			kept = append(kept, w)
		}
	}

	return strings.Join(kept, " "), fileParts
}

// buildUserMessage assembles the final user message content from:
// 1. Any staged file content (from /add, then clears it)
// 2. Inline file content resolved from file:// tokens
// 3. The user's text
func buildUserMessage(text string, inlineParts []ContentPart) any {
	var allParts []ContentPart

	if len(pendingFileContent) > 0 {
		allParts = append(allParts, pendingFileContent...)
		pendingFileContent = nil
	}

	allParts = append(allParts, inlineParts...)

	if text != "" {
		allParts = append(allParts, ContentPart{Type: "text", Text: text})
	}

	// Collapse to a plain string when everything is text — keeps history simple.
	allText := true
	for _, p := range allParts {
		if p.Type != "text" {
			allText = false
			break
		}
	}
	if allText {
		var sb strings.Builder
		for _, p := range allParts {
			sb.WriteString(p.Text)
		}
		return sb.String()
	}

	return allParts
}

// ---------------------------------------------------------------------------
// Main
// ---------------------------------------------------------------------------

var debugFile *os.File

func main() {
	_ = godotenv.Load()
	config = loadConfig()
	if config.Debug && debugFile == nil && config.DebugLevel >= "2" {
		debugFile, _ = os.OpenFile("aig_stream_debug.log", os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0666)
	}
	defer func() {
		if debugFile != nil {
			debugFile.Close()
		}
	}()

	aigDir := filepath.Join(homeDir, ".aig")
	_ = os.MkdirAll(aigDir, 0755)

	if sport := os.Getenv("STAT_SERVER_PORT"); sport != "" {
		if _port, err := strconv.Atoi(sport); err == nil {
			statServerPort = _port
		}
	}

	StartStatsServer(statServerPort)

	latestPath := getLatestContextPath(config.Model)
	if latestPath != "" {
		if err := loadHistory(latestPath, &history); err == nil {
			currentContextPath = latestPath
			historyLoaded = true
			fmt.Fprintf(os.Stderr, "🔄 Resumed context: %s\n", filepath.Base(latestPath))
		} else {
			fmt.Fprintf(os.Stderr, "Warning: Could not load context: %v\n", err)
		}
	}

	runMode := ""
	if len(os.Args) == 1 {
		// Standard REPL Mode
		fmt.Fprintln(os.Stderr, "AI Chat CLI - REPL Mode")
		fmt.Fprintln(os.Stderr, "Commands: /new, /add <file>, /r <cmd>, /exit, /history, /m <model>, /url <url> /list, /del <context>, /use <context>, /timeout <dur>, /mcp <spec>, /mcpfunc <func-name> <perm>")
		fmt.Fprintln(os.Stderr, "Use ↑/↓ to navigate history; Ctrl-R to search; type /h to see all.")
		fmt.Fprintln(os.Stderr, "Inline file attachment: include file://<path> anywhere in your message.")
		fmt.Fprintln(os.Stderr, "----------------------------------------")
		if historyLoaded {
			fmt.Fprintf(os.Stderr, "Loaded %d messages from previous session.\n", len(history)/2)
			fmt.Fprintln(os.Stderr, "Type /new to start fresh if desired.")
			fmt.Fprintln(os.Stderr)
		}

		runREPL()
	} else {
		// Non-Interactive Mode
		config.ShowThinking = false
		// config.AutoNudgeDisabled = true // Dont nudge
		runMode = handleNonInteractive(config)
	}

	if runMode != "nonit" {
		saveConfig()
		if currentContextPath != "" {
			if err := saveHistory(); err != nil {
				fmt.Fprintf(os.Stderr, "Warning: Could not save history: %v\n", err)
			}
		}
		if activeMCP != nil {
			activeMCP.Close()
		}
	}
}

// ---------------------------------------------------------------------------
// Non-interactive mode
// ---------------------------------------------------------------------------

func handleNonInteractive(config *Config) (runmode string) {
	runmode = "nonit"
	args := os.Args[1:]
	if len(args) == 0 {
		return
	}

	i := 0
	for i < len(args) {
		arg := args[i]

		isCommand := strings.HasPrefix(arg, "/") && !strings.HasPrefix(arg, "./") && !strings.HasPrefix(arg, "../")

		if !isCommand {
			fmt.Fprintf(os.Stderr, "⚠️  Unexpected argument (not a command): %s\n", arg)
			i++
			continue
		}

		cmd := arg
		i++

		switch cmd {
		case "/repl":
			config.ShowThinking = os.Getenv("SHOW_THINKING") == "on"
			runREPL()
			runmode = ""
			return

		case "/c", "/chat", "/q", "/question":
			if i >= len(args) {
				fmt.Fprintln(os.Stderr, "❌ No question provided after", cmd)
				return
			}
			rawQuestion := strings.Join(args[i:], " ")
			i = len(args)

			question, inlineContent := extractInlineFiles(rawQuestion)

			ctx, cancel := context.WithCancel(context.Background())
			sigChan := make(chan os.Signal, 1)
			signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)
			go func() {
				<-sigChan
				fmt.Print("\n⏹️  Interrupted. Stopping response...\n")
				cancel()
			}()

			if currentContextPath == "" {
				newContextName := generateContextName(config.Model, question)
				currentContextPath = filepath.Join(homeDir, ".aig", newContextName)
			}

			userMsg := buildUserMessage(question, inlineContent)

			history = append(history, Message{Role: "user", Content: userMsg})
			ans, thinking, l_history, err := askAI(ctx, *config, history)
			signal.Stop(sigChan)
			cancel()

			if err != nil {
				fmt.Fprintf(os.Stderr, "\n❌ Error: %v\n", err)
				return
			}
			history = append(l_history, Message{
				Role:     "assistant",
				Content:  ans,
				Thinking: thinking,
			})

		default:
			var cmdArgs []string
			for i < len(args) {
				next := args[i]
				nextIsCommand := strings.HasPrefix(next, "/") && !strings.HasPrefix(next, "./") && !strings.HasPrefix(next, "../")
				if nextIsCommand {
					break
				}
				cmdArgs = append(cmdArgs, next)
				i++
			}
			fullCmd := cmd
			if len(cmdArgs) > 0 {
				fullCmd = cmd + " " + strings.Join(cmdArgs, " ")
			}
			handleCommand(fullCmd, &history)
		}
	}
	return
}

// ---------------------------------------------------------------------------
// REPL — reeflective/readline
// ---------------------------------------------------------------------------

func runREPL() {
	histFile := filepath.Join(homeDir, ".aig_history_lines")

	shell := readline.NewShell()

	// Set the prompt using the correct API: shell.Prompt.Primary takes a func.
	shell.Prompt.Primary(func() string { return "> " })

	// Use the built-in file-backed history source — no custom struct needed.
	hist, err := readline.NewHistoryFromFile(histFile)
	if err != nil {
		// Non-fatal: fall back to in-memory history.
		hist = readline.NewInMemoryHistory()
	}
	shell.History.Add("aig", hist)

	// Set file path completer for commands that accept file arguments TODO fix this
	// shell.Completer = createCompleter()

	runREPLWithShell(&history, shell, histFile)
}

// ---------------------------------------------------------------------------
// Completer
// ---------------------------------------------------------------------------

func createCompleter() completer {
	// Return a function that readline calls for each argument
	return func(line []string, pos int) []string {
		if pos == 0 {
			return nil // nothing to complete on the first arg (command)
		}

		// Get the current arg (line[pos-1]) and the next arg (line[pos])
		// We want to complete line[pos] against files matching the prefix
		prefix := line[pos-1]

		// Expand ~ at the start
		expanded, err := expandHomeDir(prefix)
		if err != nil {
			return nil
		}

		// If the prefix ends with / or is empty, do a directory listing
		if strings.HasSuffix(expanded, "/") || expanded == "~" {
			// Remove trailing slash for glob
			base := strings.TrimSuffix(expanded, "/")
			pattern := filepath.Join(base, "*")
			matches, _ := filepath.Glob(pattern)
			// Sort for consistent results
			sort.Strings(matches)
			// Return only the filename part
			result := make([]string, len(matches))
			for i, m := range matches {
				result[i] = filepath.Base(m)
			}
			return result
		}

		// Otherwise, do a glob for files matching the prefix
		pattern := filepath.Join(filepath.Dir(expanded), "*")
		matches, _ := filepath.Glob(pattern)
		sort.Strings(matches)

		// Filter: only keep matches that start with the expanded prefix
		var filtered []string
		for _, m := range matches {
			if strings.HasPrefix(m, expanded) {
				filtered = append(filtered, m)
			}
		}

		// Return the suffix after the prefix (what readline expects)
		if len(filtered) > 0 {
			suffix := strings.TrimPrefix(filtered[0], expanded)
			return []string{suffix}
		}
		return nil
	}
}

type completer func([]string, int) []string

// runREPLWithShell is the main REPL loop.
func runREPLWithShell(history *[]Message, shell *readline.Shell, histFile string) {
	doTurn := func(ctx context.Context, text string) {
		cleanText, inlineContent := extractInlineFiles(text)
		userMsg := buildUserMessage(cleanText, inlineContent)

		if currentContextPath == "" {
			newContextName := generateContextName(config.Model, cleanText)
			currentContextPath = filepath.Join(homeDir, ".aig", newContextName)
		}

		*history = append(*history, Message{Role: "user", Content: userMsg})
		ans, thinking, l_history, err := askAI(ctx, *config, *history)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			*history = l_history[:len(l_history)-1]
			return
		}

		if ctx.Err() != nil {
			return
		}

		*history = append(l_history, Message{
			Role:     "assistant",
			Content:  ans,
			Thinking: thinking,
		})

		if err := saveHistory(); err != nil {
			fmt.Fprintf(os.Stderr, "⚠️  Could not save history: %v\n", err)
		}
	}

	for {
		fmt.Fprintln(os.Stderr, ">")
		line, err := shell.Readline()
		if err != nil {
			// io.EOF == Ctrl-D, readline.ErrInterrupt == Ctrl-C
			fmt.Fprintln(os.Stderr)
			break
		}

		text := strings.TrimSpace(line)
		if text == "" {
			continue
		}

		if strings.HasPrefix(text, "/") {
			if strings.HasPrefix(text, "/edit") {
				parts := strings.SplitN(text, " ", -1)
				initialMsg := ""
				if len(parts) > 1 {
					if myfile, err := expandHomeDir(parts[1]); err == nil {
						if u.FileExistsV2(myfile) == nil {
							if ctb, err := os.ReadFile(myfile); err == nil {
								initialMsg = string(ctb)
							} else {
								fmt.Fprintf(os.Stderr, "[ERROR] reading file %s\n", parts[1])
								return
							}
						}
					} else {
						idx, err := strconv.Atoi(parts[1])
						if err != nil {
							fmt.Fprintln(os.Stderr, "⚠️ Second arg should be a conversation index number. Run /h to see history.")
						} else {
							msg := (*history)[idx-1]
							switch v := msg.Content.(type) {
							case string:
								initialMsg = v
							default:
								fmt.Fprintln(os.Stderr, "⚠️ skip editing non text content")
							}
						}
					}
				}
				editorText, err := openInEditor(initialMsg)
				if err != nil {
					fmt.Fprintf(os.Stderr, "❌ Editor error: %v\n", err)
					continue
				}
				if strings.TrimSpace(editorText) == "" {
					fmt.Fprintln(os.Stderr, "⚠️  Empty input. Aborting.")
					continue
				}
				ctx, cancel := context.WithCancel(context.Background())
				sigChan := make(chan os.Signal, 1)
				signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)
				go func() { <-sigChan; cancel() }()
				doTurn(ctx, editorText)
				signal.Stop(sigChan)
				cancel()
				continue
			}

			if text == "/exit" || text == "/q" {
				return
			}

			if text == "/h" {
				printHistory(*history)
				continue
			}

			handleCommand(text, history)
			if config.Debug {
				fmt.Fprintf(os.Stderr, "[DEBUG] config: %v\n", *config)
			}
			continue
		}

		// Non-command: reeflective/readline writes to the history source
		// automatically on successful Readline() return, BUT only non-slash
		// lines reach here anyway. appendHistoryFile is still called so the
		// file stays in sync if NewHistoryFromFile fell back to in-memory.
		appendHistoryFile(histFile, text)

		ctx, cancel := context.WithCancel(context.Background())
		sigChan := make(chan os.Signal, 1)
		signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)
		go func() {
			<-sigChan
			fmt.Print("\n⏹️  Interrupted. Stopping response...\n")
			cancel()
		}()

		doTurn(ctx, text)
		signal.Stop(sigChan)
		cancel()
	}
}

// ---------------------------------------------------------------------------
// File management
// ---------------------------------------------------------------------------

func getHistoryFilePath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ".aihistory.json"
	}
	return filepath.Join(home, ".aihistory.json")
}

func loadSystemMsg(modelname string) *Message {
	promptfile := filepath.Join(homeDir, modelname+".system")
	if u.FileExistsV2(promptfile) == nil {
		fmt.Fprintln(os.Stderr, "Loading system message for "+modelname)
		if data, err := os.ReadFile(promptfile); err == nil {
			return &Message{Role: "system", Content: string(data)}
		}
	} else {
		fmt.Fprintln(os.Stderr, "[INFO] system file for this model "+promptfile+" not available")
	}
	return &Message{Role: "system", Content: ""}
}

func RemoveSystemMessage(h History) History {
	var o History
	for _, msg := range h.History {
		if msg.Role != "system" {
			o.History = append(o.History, msg)
		}
	}
	return o
}

func loadHistory(filePath string, historyPtr *[]Message) error {
	data, err := os.ReadFile(filePath)
	if err != nil {
		return err
	}
	var h History
	if err := json.Unmarshal(data, &h); err != nil {
		return err
	}
	_msg := []Message{*loadSystemMsg(config.Model)}
	_msg = append(_msg, RemoveSystemMessage(h).History...)
	h.History = _msg
	*historyPtr = h.History
	return nil
}

func appendHistoryFile(filename, line string) error {
	if line == "" || strings.HasPrefix(line, "/") {
		return nil
	}
	f, err := os.OpenFile(filename, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.WriteString(line + "\n")
	return err
}

// ---------------------------------------------------------------------------
// Command handler
// ---------------------------------------------------------------------------

func handleCommand(text string, history *[]Message) {
	parts := strings.SplitN(text, " ", -1)
	cmd := parts[0]
	arg := ""
	if len(parts) > 1 {
		arg = parts[1]
	}

	switch cmd {

	case "/trimctx":
		if arg == "" {
			fmt.Fprintln(os.Stderr, "Usage: /trimctx <index> or /trimctx <start-end> or auto auto will sue the current compression")
			fmt.Fprintln(os.Stderr, "Example: /trimctx 3-5 or /trimctx -1--3")
			return
		}
		if arg == "auto" {
			*history = trimContext(context.TODO(), *config, *history)
			return
		}

		start, end, ok := ParseRangeFromInputString(arg)
		if !ok {
			fmt.Fprintln(os.Stderr, "❌ Invalid format. Use <index> or <start-end> (e.g., 3-5-1--3)")
			return
		}

		hLen := len(*history)
		if hLen == 0 {
			fmt.Fprintln(os.Stderr, "ℹ️  History is empty.")
			return
		}

		absStart := start
		if start < 0 {
			absStart = hLen + start
		}
		absEnd := end
		if end < 0 {
			absEnd = hLen + end
		}

		minIdx, maxIdx := absStart, absEnd
		if absStart > absEnd {
			minIdx, maxIdx = absEnd, absStart
		}

		if minIdx < 0 || maxIdx >= hLen || minIdx > maxIdx {
			fmt.Fprintf(os.Stderr, "❌ Error: indices out of range (valid range: 0-%d)\n", hLen-1)
			return
		}

		removedCount := maxIdx - minIdx + 1
		newHistory := make([]Message, 0, hLen-removedCount)
		newHistory = append(newHistory, (*history)[:minIdx]...)
		newHistory = append(newHistory, (*history)[maxIdx+1:]...)
		*history = newHistory

		fmt.Fprintf(os.Stderr, "✂️  Removed %d messages (indices %d to %d).\n", removedCount, minIdx, maxIdx)

	case "/ctx":
		switch {
		case arg == "":
			if config.ContextLimit == 0 {
				fmt.Fprintln(os.Stderr, "ℹ️  Context trimming disabled. Use /ctx <tokens> to enable (e.g. /ctx 6000).")
			} else {
				fmt.Fprintf(os.Stderr, "ℹ️  Context limit: %d tokens (~%d chars). Current usage: ~%d tokens.\n",
					config.ContextLimit, config.ContextLimit*4, estimateTokens(*history))
			}
			return
		case arg == "off":
			config.ContextLimit = 0
			saveConfig()
			fmt.Fprintln(os.Stderr, "✅ Context trimming disabled.")
			return
		case arg == "sum":
			printContextSummary(*history)
			return
		default:
			n, err := strconv.Atoi(arg)
			if err != nil || n < 500 {
				fmt.Fprintln(os.Stderr, "❌ Usage: /ctx <tokens> (min 500) or /ctx off")
				return
			}
			config.ContextLimit = n
			saveConfig()
			fmt.Fprintf(os.Stderr, "✅ Context limit set to %d tokens. Current usage: ~%d tokens.\n",
				n, estimateTokens(*history))
		}

	case "/showthink":
		switch arg {
		case "on":
			config.ShowThinking = true
			saveConfig()
			fmt.Fprintln(os.Stderr, "✅ Thinking text enabled.")
		case "off":
			config.ShowThinking = false
			saveConfig()
			fmt.Fprintln(os.Stderr, "✅ Thinking text disabled.")
		default:
			fmt.Fprintln(os.Stderr, "Usage: /showthink on|off")
		}

	case "/mcpfunc":
		if arg == "" {
			fmt.Fprintln(os.Stderr, "Usage: /mcpfunc <name> <auto|ask|deny|block> - If you set block you can set the next string which is a coma sep. tools name to block. If not set the current tool name will be block")
			return
		}
		if len(parts) < 3 {
			fmt.Fprintln(os.Stderr, "❌ Error: Missing permission level. Usage: /mcpfunc <name> <auto|ask|deny|block [tool1,tool2]>")
			return
		}

		toolName := parts[1]
		perm := strings.ToLower(parts[2])

		if perm != "auto" && perm != "ask" && perm != "deny" && perm != "block" {
			fmt.Fprintln(os.Stderr, "❌ Error: Invalid permission level. Use 'auto', 'ask', 'deny', 'block'.")
			return
		}
		switch perm {
		case "block":
			blockString := arg
			if len(parts) == 4 {
				blockString = parts[3]
			}
			config.BlockedTools = blockString
			activeMCP.refreshTools()
		default:
			config.MCPPermissions[toolName] = perm
		}
		saveConfig()
		fmt.Fprintf(os.Stderr, "✅ Permission for '%s' set to [%s]\n", toolName, perm)

	case "/mcp":
		if arg == "" {
			if activeMCP == nil {
				fmt.Fprintln(os.Stderr, "ℹ️  No MCP server connected.")
				fmt.Fprintln(os.Stderr, "Usage:")
				fmt.Fprintln(os.Stderr, "  /mcp tcp://<host>:<port>    — connect to a running MCP TCP server")
				fmt.Fprintln(os.Stderr, "  /mcp <cmd> [args...]        — launch and connect to an MCP stdio server")
				fmt.Fprintln(os.Stderr, "  /mcp off                    — disconnect current MCP server")
				fmt.Fprintln(os.Stderr, "  /mcp tools                  — list available tools")
				fmt.Fprintln(os.Stderr, "  /mcp docs list              — list available resources")
				fmt.Fprintln(os.Stderr, "  /mcp docs read <uri>        — read resource contents")
			} else {
				fmt.Fprintf(os.Stderr, "✅ MCP connected: %s\n", activeMCP.Spec)
				fmt.Fprintf(os.Stderr, "   %d tool(s) available\n", len(activeMCP.Tools()))
			}
			return
		}

		switch arg {
		case "docs":
			if activeMCP == nil {
				fmt.Fprintln(os.Stderr, "ℹ️  No MCP server connected.")
				return
			}
			args := strings.SplitN(strings.TrimSpace(text[len("/mcp docs"):]), " ", 2)
			if len(args) == 0 || args[0] == "" {
				fmt.Fprintln(os.Stderr, "Usage: /mcp docs <list|read [uri]>")
				return
			}
			ops := args[0]
			switch ops {
			case "list":
				resources, err := activeMCP.Resources()
				if err != nil {
					fmt.Fprintf(os.Stderr, "❌ resources/list: %v\n", err)
					return
				}
				if len(resources) == 0 {
					fmt.Fprintln(os.Stderr, "ℹ️  No resources available.")
					return
				}
				fmt.Fprintf(os.Stderr, "📄 Available resources (%d):\n", len(resources))
				for _, r := range resources {
					fmt.Fprintf(os.Stderr, "  • %s — %s\n", r.URI, r.Name)
					if r.Description != "" {
						fmt.Fprintf(os.Stderr, "    %s\n", r.Description)
					}
				}
				return

			case "read":
				if len(args) < 2 || strings.TrimSpace(args[1]) == "" {
					fmt.Fprintln(os.Stderr, "Usage: /mcp docs read <uri>")
					return
				}
				uri := args[1]
				content, err := activeMCP.ReadResource(uri)
				if err != nil {
					fmt.Fprintf(os.Stderr, "❌ resources/read %s: %v\n", uri, err)
					return
				}
				fmt.Fprintf(os.Stderr, "📄 Resource: %s\n%s\n", uri, content)
				return

			default:
				fmt.Fprintf(os.Stderr, "❌ Unknown docs op: %q. Use 'list' or 'read [uri]'.\n", ops)
				return
			}

		case "off", "disconnect":
			if activeMCP != nil {
				activeMCP.Close()
				activeMCP = nil
				fmt.Fprintln(os.Stderr, "🔌 MCP server disconnected.")
			} else {
				fmt.Fprintln(os.Stderr, "ℹ️  No MCP server connected.")
			}
			return

		case "tools":
			if activeMCP == nil {
				fmt.Fprintln(os.Stderr, "ℹ️  No MCP server connected.")
				return
			}
			tools := activeMCP.Tools()
			if len(tools) == 0 {
				fmt.Fprintln(os.Stderr, "ℹ️  No tools available.")
				return
			}
			fmt.Fprintf(os.Stderr, "🔧 Available MCP tools (%d):\n", len(tools))
			for _, t := range tools {
				fmt.Fprintf(os.Stderr, "  • %s — %s\n", t.Name, t.Description)
			}
			return

		case "schema":
			if activeMCP == nil {
				fmt.Fprintln(os.Stderr, "ℹ️  No MCP server connected.")
				return
			}
			tools := activeMCP.Tools()
			if len(tools) == 0 {
				fmt.Fprintln(os.Stderr, "ℹ️  No tools available.")
				return
			}
			fmt.Fprintf(os.Stderr, "🔧 MCP tool schemas:\n")
			for _, t := range tools {
				fmt.Fprintf(os.Stderr, "\n  [%s]\n  Description: %s\n  InputSchema:\n", t.Name, t.Description)
				var pretty interface{}
				if err := json.Unmarshal(t.InputSchema, &pretty); err == nil {
					b, _ := json.MarshalIndent(pretty, "    ", "  ")
					fmt.Fprintf(os.Stderr, "    %s\n", string(b))
				} else {
					fmt.Fprintf(os.Stderr, "    %s\n", string(t.InputSchema))
				}
			}
			return

		case "refresh":
			if activeMCP == nil {
				fmt.Fprintln(os.Stderr, "ℹ️  No MCP server connected.")
				return
			}
			if err := activeMCP.refreshTools(); err != nil {
				fmt.Fprintf(os.Stderr, "❌ Could not refresh tools: %v\n", err)
			} else {
				fmt.Fprintf(os.Stderr, "✅ Refreshed: %d tool(s)\n", len(activeMCP.Tools()))
			}
			return
		}

		// No sub-command matched — treat rest as MCP server spec to connect
		if activeMCP != nil {
			fmt.Fprintln(os.Stderr, "🔌 Disconnecting previous MCP server...")
			activeMCP.Close()
			activeMCP = nil
		}

		var newMCP *ResilientMCPClient
		var err error

		switch {
		case strings.HasPrefix(arg, "http"):
			raw, e := ConnectStreamableHTTP(arg)
			if e != nil {
				fmt.Fprintf(os.Stderr, "❌ MCP connect failed: %v\n", e)
				return
			}
			newMCP, err = NewResilientPassthrough(raw), e
		default:
			fmt.Fprintf(os.Stderr, "🚀 Launching MCP stdio server: %s\n", arg)
			newMCP, err = NewResilientStdio(parts[1:])
		}

		if err != nil {
			fmt.Fprintf(os.Stderr, "❌ MCP connect failed: %v\n", err)
			return
		}

		activeMCP = newMCP
		tools := activeMCP.Tools()
		fmt.Fprintf(os.Stderr, "✅ MCP connected: %s\n", activeMCP.Spec)
		fmt.Fprintf(os.Stderr, "   %d tool(s) available:\n", len(tools))
		for _, t := range tools {
			fmt.Fprintf(os.Stderr, "   • %s — %s\n", t.Name, t.Description)
		}

	case "/s":
		if arg == "" {
			fmt.Fprintln(os.Stderr, "Usage: /s <index:filename>")
			return
		}
		idx_file := strings.Split(arg, ":")
		if len(idx_file) != 2 {
			fmt.Fprintf(os.Stderr, "error input must be idx:file-path")
			return
		}
		idxStr, filePath := idx_file[0], idx_file[1]
		expandedPath, err := expandHomeDir(filePath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "⚠️  Could not resolve path '%s': %v\n", filePath, err)
			return
		}
		idx, err := strconv.Atoi(idxStr)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error, index must be integer")
			return
		}
		if err := saveHistoryToFile(*history, idx, expandedPath); err != nil {
			fmt.Fprintf(os.Stderr, "❌ Failed to save: %v\n", err)
		} else {
			fmt.Fprintf(os.Stderr, "✅ Saved conversation up to index %d to: %s\n", idx, expandedPath)
		}

	case "/cd":
		if arg == "" {
			fmt.Fprintln(os.Stderr, "Usage: /cd <directory>")
			return
		}
		if err := os.Chdir(arg); err != nil {
			fmt.Fprintf(os.Stderr, "[ERROR] can not chdir to %s - %v\n", arg, err)
		}

	case "/new", "/n":
		if len(*history) > 0 {
			firstUserMsg := ""
			for _, m := range *history {
				if m.Role == "user" {
					if s, ok := m.Content.(string); ok {
						firstUserMsg = s
						break
					}
					break
				}
			}
			if firstUserMsg == "" {
				firstUserMsg = "untitled"
			}

			oldName := generateContextName(config.Model, firstUserMsg)
			if err := saveHistory(); err != nil {
				fmt.Fprintf(os.Stderr, "⚠️  Could not save current session: %v\n", err)
			} else {
				fmt.Fprintf(os.Stderr, "✅ Saved current session to: %s\n", oldName)
			}
		}

		*history = []Message{}
		pendingFileContent = nil

		if arg != "" {
			newName := generateContextName(config.Model, arg)
			currentContextPath = filepath.Join(homeDir, ".aig", newName)
			fmt.Fprintf(os.Stderr, "✅ New context started with custom name: %s\n", newName)
		} else {
			currentContextPath = ""
			fmt.Fprintln(os.Stderr, "✅ New context started (name will be based on first question).")
		}

	case "/help":
		fmt.Fprintln(os.Stderr, "Commands:")
		fmt.Fprintln(os.Stderr, "  /new , /n                     - Clear conversation history")
		fmt.Fprintln(os.Stderr, "  /edit,                        - Open EDITOR to edit user message")
		fmt.Fprintln(os.Stderr, "  /edit <index>,                - Open EDITOR to edit user message using existing conversation index")
		fmt.Fprintln(os.Stderr, "  /s <hist_index:filename>      - Save the history index to a file")
		fmt.Fprintln(os.Stderr, "  /history or /h                - Show current chat history")
		fmt.Fprintln(os.Stderr, "  /list, /l                     - List contexts for current model")
		fmt.Fprintln(os.Stderr, "  /add <file>,/a                - Stage file for next message (user role)")
		fmt.Fprintln(os.Stderr, "  /addsystem <file>,/as         - Stage file for system message (system role)")
		fmt.Fprintln(os.Stderr, "  /r <cmd>                      - Run shell command and show output")
		fmt.Fprintln(os.Stderr, "  /m <model>                    - Switch model (e.g., /m gpt-4). To list models run /m list or /m ls")
		fmt.Fprintln(os.Stderr, "  /ms <model-summary>           - Switch model used for context summary to comress context")
		fmt.Fprintln(os.Stderr, "  /msurl <model-summary-URL>    - Switch the api endpoint for summary model. If empty the global one will be used")
		fmt.Fprintln(os.Stderr, "  /msto <number-in-secs>        - Set model summary timeout (e.g., 300 - which 300 secs)")
		fmt.Fprintln(os.Stderr, "  /url <url>                    - Switch API URL")
		fmt.Fprintln(os.Stderr, "  /timeout or /t <dur>          - Set request timeout (e.g., 30s, 5m, 1h)")
		fmt.Fprintln(os.Stderr, "  /exit or /q                   - Exit REPL")
		fmt.Fprintln(os.Stderr, "  /use <name>                   - Switch to an existing context")
		fmt.Fprintln(os.Stderr, "  /del <name>                   - Delete specific context")
		fmt.Fprintln(os.Stderr, "  /del all                      - Delete all contexts for current model")
		fmt.Fprintln(os.Stderr, "  /debug <0|1|2>                - Enable/Disable debug and set debug level")
		fmt.Fprintln(os.Stderr, "  /show <thing>                 - Show details (e.g., /show context <name>)")
		fmt.Fprintln(os.Stderr, "  /showthink <on|off>           - Show thinking process. Default is off")
		fmt.Fprintln(os.Stderr, "  /cd <dirname>                 - Change to directory")
		fmt.Fprintln(os.Stderr, "  /mcp <spec>                   - Connect MCP server (tcp://host:port or cmd)")
		fmt.Fprintln(os.Stderr, "  /mcp off                      - Disconnect MCP server")
		fmt.Fprintln(os.Stderr, "  /mcp tools                    - List available MCP tools")
		fmt.Fprintln(os.Stderr, "  /mcp refresh                  - Refresh MCP tool list")
		fmt.Fprintln(os.Stderr, "  /mcpfunc <func> <perm>        - Set permission for tools func, auto|denied|ask")
		fmt.Fprintln(os.Stderr, "  /ctx <N>|off                  - Set context token limit (auto-trim when exceeded)")
		fmt.Fprintln(os.Stderr, "  /maxtoken <N>                 - Set max tokens in payload.")
		fmt.Fprintln(os.Stderr, "  /trimctx <idx|range>          - Remove messages at index or range (e.g. 3-5, -1--3)")
		fmt.Fprintln(os.Stderr, "  /autonudgedisabled <on|off>   - Disable the auto nudge feature")
		fmt.Fprintln(os.Stderr, "  /configdir dir://<newdir>     - Switch config directory (.aigdotenv and .aig/). If run from from start add prefix dir:// so we dont treat the / as next command. eg. aig /n /configdir dir:///home/user/aig1 /repl - Within a session it is not required. ~ will be expanded into home dir")
		fmt.Fprintln(os.Stderr)
		fmt.Fprintf(os.Stderr, "Curl mode: using curl http://localhost:%d as base for these below cmds\n", statServerPort)
		fmt.Fprintln(os.Stderr, "  currently only reporting stats")
		fmt.Fprintln(os.Stderr, "Inline file attachment:")
		fmt.Fprintln(os.Stderr, "  Include file://<path> anywhere in your message to attach a file inline.")
		fmt.Fprintln(os.Stderr, "  Example: Summarise this file:/home/user/notes.txt please")

	case "/use":
		if arg == "" {
			fmt.Fprintln(os.Stderr, "Usage: /use <context-name>")
			return
		}
		sanitizedName := strings.ReplaceAll(arg, " ", "_")
		path := filepath.Join(filepath.Join(homeDir, ".aig"), sanitizedName)
		if _, err := os.Stat(path); os.IsNotExist(err) {
			fmt.Fprintf(os.Stderr, "Error: context '%s' not found.\n", arg)
			return
		}
		if err := loadHistory(path, history); err != nil {
			fmt.Fprintf(os.Stderr, "Error loading context: %v\n", err)
			return
		}
		currentContextPath = path
		fmt.Fprintf(os.Stderr, "✅ Switched to context: %s\n", arg)

	case "/list", "/l":
		fmt.Fprintf(os.Stderr, "📜 Contexts for model [%s]:\n", config.Model)
		files, _ := os.ReadDir(filepath.Join(homeDir, ".aig"))
		found := false
		for _, f := range files {
			if !f.IsDir() {
				fmt.Fprintf(os.Stderr, "  - %s\n", f.Name())
				found = true
			}
		}
		if !found {
			fmt.Fprintln(os.Stderr, "  (No contexts found)")
		}
		fmt.Fprintf(os.Stderr, " Current context: %s\n", filepath.Base(currentContextPath))

	case "/del":
		if arg == "" {
			fmt.Fprintln(os.Stderr, "Usage: /del <name> or /del all")
			return
		}
		if arg == "all" {
			files, _ := os.ReadDir(filepath.Join(homeDir, ".aig"))
			count := 0
			for _, f := range files {
				if !f.IsDir() && strings.HasSuffix(f.Name(), "_"+config.Model+".json") {
					os.Remove(filepath.Join(filepath.Join(homeDir, ".aig"), f.Name()))
					count++
				}
			}
			fmt.Fprintf(os.Stderr, "🗑️ Deleted %d contexts.\n", count)
		} else {
			path := filepath.Join(filepath.Join(homeDir, ".aig"), arg+".json")
			if err := os.Remove(path); err != nil {
				fmt.Fprintf(os.Stderr, "Error deleting context: %v\n", err)
			} else {
				fmt.Fprintf(os.Stderr, "✅ Deleted context: %s\n", arg)
				if currentContextPath != "" && strings.Contains(currentContextPath, arg) {
					currentContextPath = ""
					*history = []Message{}
				}
			}
		}

	case "/debug":
		if arg == "" {
			fmt.Fprintln(os.Stderr, "Usage: /debug <0|1|2>")
			return
		}
		switch arg {
		case "0", "off":
			config.Debug = false
		default:
			config.Debug = true
			config.DebugLevel = strings.TrimSpace(arg)
		}
		if config.Debug && debugFile == nil {
			debugFile, _ = os.OpenFile("aig_stream_debug.log", os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0666)
		}

	case "/system", "/sys":
		if arg == "" {
			fmt.Fprintln(os.Stderr, "Usage: /system <text> It will create a new session. If text has file://<model-name> it will load the file <model-name>.system from the the current config directory. Normally at start the program already loaded for the corresponding model or when you switch model if these files are exist. This option allow you to load other model system to use for the current one")
			if len(*history) > 0 {
				fmt.Fprintf(os.Stderr, "Current %s prompt: '%s'\n", (*history)[0].Role, (*history)[0].Content)
			}
			return
		}
		var systemMsg *Message
		if strings.HasPrefix(arg, "file://") {
			systemMsg = loadSystemMsg(strings.TrimPrefix(arg, "file://"))
		} else {
			systemprompt := strings.Join(parts[1:], " ")
			systemMsg = &Message{Role: "system", Content: systemprompt}
		}
		os.WriteFile(filepath.Join(homeDir, config.Model+".system"), []byte(systemMsg.Content.(string)), 0o640)
		fmt.Fprintln(os.Stderr, "✅ System prompt added")
		*history = []Message{*systemMsg}

	case "/add", "/a":
		if arg == "" {
			fmt.Fprintln(os.Stderr, "Usage: /add <filename>")
			return
		}
		expandedPath, err := expandHomeDir(arg)
		if err != nil {
			fmt.Fprintf(os.Stderr, "⚠️  Could not resolve path '%s': %v\n", arg, err)
			return
		}
		parts, err := processFile(expandedPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error processing file %s: %v\n", expandedPath, err)
			return
		}
		pendingFileContent = append(pendingFileContent, parts...)
		fmt.Fprintf(os.Stderr, "📎 Staged '%s' — will be included in your next message.\n", arg)
		fmt.Fprintf(os.Stderr, "   (Total staged parts: %d)\n", len(pendingFileContent))

	case "/addsystem", "/as":
		if arg == "" {
			fmt.Fprintln(os.Stderr, "Usage: /addsystem | /as <filename>. Add the file content to the system message")
			return
		}
		expandedPath, err := expandHomeDir(arg)
		if err != nil {
			fmt.Fprintf(os.Stderr, "⚠️  Could not resolve path '%s': %v\n", arg, err)
			return
		}
		parts, err := processFile(expandedPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error processing file %s: %v\n", expandedPath, err)
			return
		}
		var content any
		if len(parts) == 1 && parts[0].Type == "text" {
			content = parts[0].Text
		} else {
			content = parts
		}
		*history = []Message{
			{Role: "system", Content: content},
		}

	case "/r":
		if arg == "" {
			fmt.Fprintln(os.Stderr, "Usage: /r <command>")
			return
		}
		runSystemCommand(strings.Join(parts[1:], " "))

	case "/m":
		if arg == "" {
			fmt.Fprintln(os.Stderr, "Usage: /m <model>")
			return
		}
		switch arg {
		case "list", "ls":
			url, _ := url.Parse(config.BaseURL)
			if o, err := u.Curl("GET", fmt.Sprintf("%s://%s/v1/models", url.Scheme, url.Host), "", "", []string{"Authorization: Bearer " + config.APIKey}, nil); err == nil {
				v := OpenAIModelsResponse{}
				if err1 := json.Unmarshal([]byte(o), &v); err1 == nil {
					fmt.Fprintf(os.Stderr, "%s\n", u.JsonDump(v, ""))
				} else {
					fmt.Fprintf(os.Stderr, "%s\n", err1.Error())
				}
			} else {
				fmt.Fprintf(os.Stderr, "Failed to query models list %s\n", err.Error())
			}
			return
		default:
			config.Model = arg
			fmt.Fprintf(os.Stderr, "Model switched to: %s\n", arg)
		}

	case "/ms":
		if arg == "" {
			fmt.Fprintln(os.Stderr, "Usage: /ms <summary-model>")
			return
		}
		config.SummaryModel = arg
		fmt.Fprintf(os.Stderr, "Summary Model switched to: %s\n", arg)

	case "/msto":
		if arg == "" {
			fmt.Fprintln(os.Stderr, "Usage: /msto <summary-model-timout> eg. 300s")
			return
		}
		config.SummaryModelTimeout = arg
		fmt.Fprintf(os.Stderr, "Summary Model Timeout switched to: %s\n", arg)
	case "/msurl":
		if arg == "" {
			fmt.Fprintln(os.Stderr, "Usage: /msurl <summary-model-url>. By default if empty it uses the same endpoint as global url")
			return
		}
		config.SummaryModelUrl = arg
		fmt.Fprintf(os.Stderr, "Summary Model URL switched to: %s\n", arg)

	case "/url":
		if arg == "" {
			fmt.Fprintln(os.Stderr, "Usage: /url <url>")
			return
		}
		config.BaseURL = arg
		fmt.Fprintf(os.Stderr, "URL switched to: %s\n", arg)

	case "/timeout", "/t":
		if arg == "" {
			fmt.Fprintln(os.Stderr, "Usage: /timeout <duration> (e.g., 30s, 5m, 1h)")
			return
		}
		d, err := time.ParseDuration(arg)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error parsing duration: %v\n", err)
			return
		}
		config.Timeout = d
		fmt.Fprintf(os.Stderr, "Timeout set to: %v\n", d)

	case "/p":
		fmt.Fprintf(os.Stderr, "Current config: %s\n", u.JsonDump(config, ""))
		if activeMCP != nil {
			fmt.Fprintf(os.Stderr, "MCP: %s (%d tools)\n", activeMCP.Spec, len(activeMCP.Tools()))
		} else {
			fmt.Fprintln(os.Stderr, "MCP: not connected")
		}

	case "/show":
		if arg == "" {
			fmt.Fprintln(os.Stderr, "Usage: /show <thing> (e.g., /show context <name>)")
			return
		}
		if strings.HasPrefix(arg, "context ") {
			contextName := strings.TrimPrefix(arg, "context ")
			path := filepath.Join(homeDir, ".aig", contextName)
			if _, err := os.Stat(path); os.IsNotExist(err) {
				fmt.Fprintf(os.Stderr, "Error: context '%s' not found.\n", contextName)
				return
			}
			var tempHistory []Message
			if err := loadHistory(path, &tempHistory); err != nil {
				fmt.Fprintf(os.Stderr, "Error loading context: %v\n", err)
				return
			}
			fmt.Fprintf(os.Stderr, "📜 Showing context: %s\n", contextName)
			fmt.Fprintln(os.Stderr, strings.Repeat("-", 20))
			for _, msg := range tempHistory {
				switch msg.Role {
				case "user":
					fmt.Fprintf(os.Stderr, "user: %s\n", msg.Content)
				case "assistant":
					fmt.Fprintf(os.Stderr, "AI: %s\n", msg.Content)
				}
			}
			fmt.Fprintln(os.Stderr, strings.Repeat("-", 20))
		} else {
			fmt.Fprintf(os.Stderr, "Unknown thing to show: %s. Try '/show context <name>'\n", arg)
		}

	case "/configdir":
		arg = strings.TrimPrefix(arg, "dir://")
		if arg == "" {
			panic("Usage: /configdir dir://<newdir>. Understand ~ expansion into user home dir")
		}
		if currentContextPath != "" {
			saveHistory()
		}
		saveConfig()
		arg, _ = expandHomeDir(arg)
		absPath, err := filepath.Abs(arg)
		if err != nil {
			fmt.Fprintf(os.Stderr, "❌ Error resolving path: %v\n", err)
			return
		}
		homeDir = absPath
		_ = os.MkdirAll(homeDir, 0755)
		fmt.Fprintln(os.Stderr, "Re-load config")
		currentContextPath = getLatestContextPath(config.Model)
		config = loadConfig()
		fmt.Fprintf(os.Stderr, u.JsonDump(config, ""))
		historyLoaded = false
		*history = []Message{}

		fmt.Fprintf(os.Stderr, "✅ Config directory switched to: %s\n", homeDir)
	case "/maxtoken":
		if arg == "" {
			fmt.Fprintln(os.Stderr, "Usage: /maxtoken <int>")
			return
		}
		if maxtok, err := strconv.Atoi(arg); err != nil {
			fmt.Fprintf(os.Stderr, "Can not parse int: %s\n", err.Error())
			return
		} else {
			config.MaxTokens = maxtok
			os.Setenv("MAX_TOKENS", arg)
			fmt.Fprintf(os.Stderr, "Maxtokens set to: %d\n", config.MaxTokens)
			saveConfig()
		}
	case "/autonudgedisabled":
		if arg == "" {
			fmt.Fprintln(os.Stderr, "Usage: /maxtoken <int>")
		}
		switch arg {
		case "on":
			fmt.Fprintln(os.Stderr, "Disable autonudge")
			config.AutoNudgeDisabled = true
		default:
			fmt.Fprintln(os.Stderr, "Enable autonudge")
			config.AutoNudgeDisabled = false
		}

	case "/unload":
		if arg == "" {
			fmt.Fprintln(os.Stderr, "Usage: /unload <model-name> - Unload a model from the backend")
			fmt.Fprintln(os.Stderr, "Example: /unload gpt-3.5-turbo")
			return
		}
		modelName := strings.TrimSpace(arg)
		if modelName == "" {
			fmt.Fprintln(os.Stderr, "❌ No model name provided.")
			return
		}
		url, _ := url.Parse(config.BaseURL)
		// Build the POST request body
		bodyBytes, _ := json.Marshal(map[string]string{"model": modelName})
		// Make the unload request
		if _, err := u.Curl("POST", fmt.Sprintf("%s://%s/models/unload", url.Scheme, url.Host), string(bodyBytes), "application/json", []string{"Content-Type: application/json"}, nil); err == nil {
			fmt.Fprintf(os.Stderr, "✅ Model unloaded: %s\n", modelName)
		} else {
			fmt.Fprintf(os.Stderr, "❌ Failed to unload model %s: %v\n", modelName, err)
		}

	default:
		fmt.Fprintf(os.Stderr, "Unknown command: %s\n", cmd)
	}
}

// ---------------------------------------------------------------------------
// openInEditor — unchanged
// ---------------------------------------------------------------------------

func openInEditor(initialText string) (string, error) {
	editor := os.Getenv("EDITOR")
	if editor == "" {
		editor = os.Getenv("VISUAL")
	}
	if editor == "" {
		editor = "vim"
	}

	tmpFile, err := os.CreateTemp("", "aig-edit-*.txt")
	if err != nil {
		return "", fmt.Errorf("failed to create temp file: %v", err)
	}
	defer os.Remove(tmpFile.Name())

	if _, err := tmpFile.WriteString(initialText); err != nil {
		return "", fmt.Errorf("failed to write to temp file: %v", err)
	}
	tmpFile.Close()

	cmd := exec.Command("sh", "-c", fmt.Sprintf("%s %s", editor, tmpFile.Name()))
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("editor exited with error: %v", err)
	}

	content, err := os.ReadFile(tmpFile.Name())
	if err != nil {
		return "", fmt.Errorf("failed to read temp file: %v", err)
	}

	return string(content), nil
}

// ---------------------------------------------------------------------------
// runREPLFallback — plain bufio loop for non-TTY / piped input
// ---------------------------------------------------------------------------

func runREPLFallback(history *[]Message) {
	scanner := bufio.NewScanner(os.Stdin)
	for {
		fmt.Print("> ")
		if !scanner.Scan() {
			break
		}
		text := strings.TrimSpace(scanner.Text())
		if text == "" {
			continue
		}
		if text == "/exit" || text == "/q" {
			return
		}
		if text == "/h" {
			printHistory(*history)
			continue
		}
		if strings.HasPrefix(text, "/") {
			handleCommand(text, history)
			continue
		}

		cleanText, inlineContent := extractInlineFiles(text)
		userMsg := buildUserMessage(cleanText, inlineContent)

		if currentContextPath == "" {
			newContextName := generateContextName(config.Model, cleanText)
			currentContextPath = filepath.Join(homeDir, ".aig", newContextName)
		}

		ctx := context.Background()
		*history = append(*history, Message{Role: "user", Content: userMsg})
		ans, thinking, l_history, err := askAI(ctx, *config, *history)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			*history = l_history[:len(l_history)-1]
			continue
		}
		*history = append(l_history, Message{
			Role:     "assistant",
			Content:  ans,
			Thinking: thinking,
		})
	}
}
