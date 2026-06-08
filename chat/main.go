package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
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
				fmt.Printf("⚠️  Could not load file %s: %v\n", path, err)
				kept = append(kept, w)
			} else {
				fmt.Printf("📎 Loaded inline file: %s\n", path)
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
			fmt.Printf("🔄 Resumed context: %s\n", filepath.Base(latestPath))
		} else {
			fmt.Fprintf(os.Stderr, "Warning: Could not load context: %v\n", err)
		}
	}

	runMode := ""
	if len(os.Args) == 1 {
		// Standard REPL Mode
		fmt.Println("AI Chat CLI - REPL Mode")
		fmt.Println("Commands: /new, /add <file>, /r <cmd>, /exit, /history, /m <model>, /url <url> /list, /del <context>, /use <context>, /timeout <dur>, /mcp <spec>, /mcpfunc <func-name> <perm>")
		fmt.Println("Use ↑/↓ to navigate history; Ctrl-R to search; type /h to see all.")
		fmt.Println("Inline file attachment: include file://<path> anywhere in your message.")
		fmt.Println("----------------------------------------")
		if historyLoaded {
			fmt.Printf("Loaded %d messages from previous session.\n", len(history)/2)
			fmt.Println("Type /new to start fresh if desired.")
			fmt.Println()
		}

		runREPL()
	} else {
		// Non-Interactive Mode
		config.ShowThinking = false
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

	runREPLWithShell(&history, shell, histFile)
}

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
			fmt.Printf("Error: %v\n", err)
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
		fmt.Println(">")
		line, err := shell.Readline()
		if err != nil {
			// io.EOF == Ctrl-D, readline.ErrInterrupt == Ctrl-C
			fmt.Println()
			break
		}

		text := strings.TrimSpace(line)
		if text == "" {
			continue
		}

		if strings.HasPrefix(text, "/") {
			// ------------------------------------------------------------------
			// /edit [index] — open $EDITOR
			// ------------------------------------------------------------------
			if strings.HasPrefix(text, "/edit") {
				parts := strings.SplitN(text, " ", -1)
				initialMsg := ""
				if len(parts) > 1 {
					idx, err := strconv.Atoi(parts[1])
					if err != nil {
						fmt.Println("⚠️ Second arg should be a conversation index number. Run /h to see history.")
					} else {
						msg := (*history)[idx-1]
						switch v := msg.Content.(type) {
						case string:
							initialMsg = v
						default:
							fmt.Println("⚠️ skip editing non text content")
						}
					}
				}
				editorText, err := openInEditor(initialMsg)
				if err != nil {
					fmt.Printf("❌ Editor error: %v\n", err)
					continue
				}
				if strings.TrimSpace(editorText) == "" {
					fmt.Println("⚠️  Empty input. Aborting.")
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
				fmt.Printf("[DEBUG] config: %v\n", *config)
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

func loadHistory(filePath string, historyPtr *[]Message) error {
	data, err := os.ReadFile(filePath)
	if err != nil {
		return err
	}
	var h History
	if err := json.Unmarshal(data, &h); err != nil {
		return err
	}
	promptfile := filepath.Join(homeDir, config.Model+".system")
	if u.FileExistsV2(promptfile) == nil {
		fmt.Println("Loading system message for " + config.Model)
		if data, err := os.ReadFile(promptfile); err == nil {
			sysMsg := Message{Role: "system", Content: string(data)}
			// appends h.History elements directly onto the new single-element slice
			h.History = append([]Message{sysMsg}, h.History...)
		}
	}
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
			fmt.Println("Usage: /trimctx <index> or /trimctx <start-end>")
			fmt.Println("Example: /trimctx 3-5 or /trimctx -1--3")
			return
		}
		start, end, ok := ParseRangeFromInputString(arg)
		if !ok {
			fmt.Println("❌ Invalid format. Use <index> or <start-end> (e.g., 3-5-1--3)")
			return
		}

		hLen := len(*history)
		if hLen == 0 {
			fmt.Println("ℹ️  History is empty.")
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
			fmt.Printf("❌ Error: indices out of range (valid range: 0-%d)\n", hLen-1)
			return
		}

		removedCount := maxIdx - minIdx + 1
		newHistory := make([]Message, 0, hLen-removedCount)
		newHistory = append(newHistory, (*history)[:minIdx]...)
		newHistory = append(newHistory, (*history)[maxIdx+1:]...)
		*history = newHistory

		fmt.Printf("✂️  Removed %d messages (indices %d to %d).\n", removedCount, minIdx, maxIdx)

	case "/ctx":
		switch {
		case arg == "":
			if config.ContextLimit == 0 {
				fmt.Println("ℹ️  Context trimming disabled. Use /ctx <tokens> to enable (e.g. /ctx 6000).")
			} else {
				fmt.Printf("ℹ️  Context limit: %d tokens (~%d chars). Current usage: ~%d tokens.\n",
					config.ContextLimit, config.ContextLimit*4, estimateTokens(*history))
			}
			return
		case arg == "off":
			config.ContextLimit = 0
			saveConfig()
			fmt.Println("✅ Context trimming disabled.")
			return
		case arg == "sum":
			printContextSummary(*history)
			return
		default:
			n, err := strconv.Atoi(arg)
			if err != nil || n < 500 {
				fmt.Println("❌ Usage: /ctx <tokens> (min 500) or /ctx off")
				return
			}
			config.ContextLimit = n
			saveConfig()
			fmt.Printf("✅ Context limit set to %d tokens. Current usage: ~%d tokens.\n",
				n, estimateTokens(*history))
		}

	case "/showthink":
		switch arg {
		case "on":
			config.ShowThinking = true
			saveConfig()
			fmt.Println("✅ Thinking text enabled.")
		case "off":
			config.ShowThinking = false
			saveConfig()
			fmt.Println("✅ Thinking text disabled.")
		default:
			fmt.Println("Usage: /showthink on|off")
		}

	case "/mcpfunc":
		if arg == "" {
			fmt.Println("Usage: /mcpfunc <name> <auto|ask|deny|block> - If you set block you can set the next string which is a coma sep. tools name to block. If not set the current tool name will be block")
			return
		}
		if len(parts) < 3 {
			fmt.Println("❌ Error: Missing permission level. Usage: /mcpfunc <name> <auto|ask|deny|block [tool1,tool2]>")
			return
		}

		toolName := parts[1]
		perm := strings.ToLower(parts[2])

		if perm != "auto" && perm != "ask" && perm != "deny" && perm != "block" {
			fmt.Println("❌ Error: Invalid permission level. Use 'auto', 'ask', 'deny', 'block'.")
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
		fmt.Printf("✅ Permission for '%s' set to [%s]\n", toolName, perm)

	case "/mcp":
		if arg == "" {
			if activeMCP == nil {
				fmt.Println("ℹ️  No MCP server connected.")
				fmt.Println("Usage:")
				fmt.Println("  /mcp tcp://<host>:<port>    — connect to a running MCP TCP server")
				fmt.Println("  /mcp <cmd> [args...]        — launch and connect to an MCP stdio server")
				fmt.Println("  /mcp off                    — disconnect current MCP server")
				fmt.Println("  /mcp tools                  — list available tools")
				fmt.Println("  /mcp docs list              — list available resources")
				fmt.Println("  /mcp docs read <uri>        — read resource contents")
			} else {
				fmt.Printf("✅ MCP connected: %s\n", activeMCP.Spec)
				fmt.Printf("   %d tool(s) available\n", len(activeMCP.Tools()))
			}
			return
		}

		switch arg {
		case "docs":
			if activeMCP == nil {
				fmt.Println("ℹ️  No MCP server connected.")
				return
			}
			args := strings.SplitN(strings.TrimSpace(text[len("/mcp docs"):]), " ", 2)
			if len(args) == 0 || args[0] == "" {
				fmt.Println("Usage: /mcp docs <list|read [uri]>")
				return
			}
			ops := args[0]
			switch ops {
			case "list":
				resources, err := activeMCP.Resources()
				if err != nil {
					fmt.Printf("❌ resources/list: %v\n", err)
					return
				}
				if len(resources) == 0 {
					fmt.Println("ℹ️  No resources available.")
					return
				}
				fmt.Printf("📄 Available resources (%d):\n", len(resources))
				for _, r := range resources {
					fmt.Printf("  • %s — %s\n", r.URI, r.Name)
					if r.Description != "" {
						fmt.Printf("    %s\n", r.Description)
					}
				}
				return

			case "read":
				if len(args) < 2 || strings.TrimSpace(args[1]) == "" {
					fmt.Println("Usage: /mcp docs read <uri>")
					return
				}
				uri := args[1]
				content, err := activeMCP.ReadResource(uri)
				if err != nil {
					fmt.Printf("❌ resources/read %s: %v\n", uri, err)
					return
				}
				fmt.Printf("📄 Resource: %s\n%s\n", uri, content)
				return

			default:
				fmt.Printf("❌ Unknown docs op: %q. Use 'list' or 'read [uri]'.\n", ops)
				return
			}

		case "off", "disconnect":
			if activeMCP != nil {
				activeMCP.Close()
				activeMCP = nil
				fmt.Println("🔌 MCP server disconnected.")
			} else {
				fmt.Println("ℹ️  No MCP server connected.")
			}
			return

		case "tools":
			if activeMCP == nil {
				fmt.Println("ℹ️  No MCP server connected.")
				return
			}
			tools := activeMCP.Tools()
			if len(tools) == 0 {
				fmt.Println("ℹ️  No tools available.")
				return
			}
			fmt.Printf("🔧 Available MCP tools (%d):\n", len(tools))
			for _, t := range tools {
				fmt.Printf("  • %s — %s\n", t.Name, t.Description)
			}
			return

		case "schema":
			if activeMCP == nil {
				fmt.Println("ℹ️  No MCP server connected.")
				return
			}
			tools := activeMCP.Tools()
			if len(tools) == 0 {
				fmt.Println("ℹ️  No tools available.")
				return
			}
			fmt.Printf("🔧 MCP tool schemas:\n")
			for _, t := range tools {
				fmt.Printf("\n  [%s]\n  Description: %s\n  InputSchema:\n", t.Name, t.Description)
				var pretty interface{}
				if err := json.Unmarshal(t.InputSchema, &pretty); err == nil {
					b, _ := json.MarshalIndent(pretty, "    ", "  ")
					fmt.Printf("    %s\n", string(b))
				} else {
					fmt.Printf("    %s\n", string(t.InputSchema))
				}
			}
			return

		case "refresh":
			if activeMCP == nil {
				fmt.Println("ℹ️  No MCP server connected.")
				return
			}
			if err := activeMCP.refreshTools(); err != nil {
				fmt.Printf("❌ Could not refresh tools: %v\n", err)
			} else {
				fmt.Printf("✅ Refreshed: %d tool(s)\n", len(activeMCP.Tools()))
			}
			return
		}

		// No sub-command matched — treat rest as MCP server spec to connect
		if activeMCP != nil {
			fmt.Println("🔌 Disconnecting previous MCP server...")
			activeMCP.Close()
			activeMCP = nil
		}

		var newMCP *ResilientMCPClient
		var err error

		switch {
		case strings.HasPrefix(arg, "http"):
			raw, e := ConnectStreamableHTTP(arg)
			if e != nil {
				fmt.Printf("❌ MCP connect failed: %v\n", e)
				return
			}
			newMCP, err = NewResilientPassthrough(raw), e
		default:
			fmt.Printf("🚀 Launching MCP stdio server: %s\n", arg)
			newMCP, err = NewResilientStdio(parts[1:])
		}

		if err != nil {
			fmt.Printf("❌ MCP connect failed: %v\n", err)
			return
		}

		activeMCP = newMCP
		tools := activeMCP.Tools()
		fmt.Printf("✅ MCP connected: %s\n", activeMCP.Spec)
		fmt.Printf("   %d tool(s) available:\n", len(tools))
		for _, t := range tools {
			fmt.Printf("   • %s — %s\n", t.Name, t.Description)
		}

	case "/s":
		if arg == "" {
			fmt.Println("Usage: /s <index:filename>")
			return
		}
		idx_file := strings.Split(arg, ":")
		if len(idx_file) != 2 {
			println("error input must be idx:file-path")
			return
		}
		idxStr, path := idx_file[0], idx_file[1]
		idx, err := strconv.Atoi(idxStr)
		if err != nil {
			println("error, index must be integer")
			return
		}
		if err := saveHistoryToFile(*history, idx, path); err != nil {
			fmt.Printf("❌ Failed to save: %v\n", err)
		} else {
			fmt.Printf("✅ Saved conversation up to index %d to: %s\n", idx, path)
		}

	case "/cd":
		if arg == "" {
			fmt.Println("Usage: /cd <directory>")
			return
		}
		if err := os.Chdir(arg); err != nil {
			fmt.Printf("[ERROR] can not chdir to %s - %v\n", arg, err)
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
				fmt.Printf("⚠️  Could not save current session: %v\n", err)
			} else {
				fmt.Printf("✅ Saved current session to: %s\n", oldName)
			}
		}

		*history = []Message{}
		pendingFileContent = nil

		if arg != "" {
			newName := generateContextName(config.Model, arg)
			currentContextPath = filepath.Join(homeDir, ".aig", newName)
			fmt.Printf("✅ New context started with custom name: %s\n", newName)
		} else {
			currentContextPath = ""
			fmt.Println("✅ New context started (name will be based on first question).")
		}

	case "/help":
		fmt.Println("Commands:")
		fmt.Println("  /new , /n                     - Clear conversation history")
		fmt.Println("  /edit,                        - Open EDITOR to edit user message")
		fmt.Println("  /edit <index>,                - Open EDITOR to edit user message using existing conversation index")
		fmt.Println("  /s <hist_index:filename>      - Save the history index to a file")
		fmt.Println("  /history or /h                - Show current chat history")
		fmt.Println("  /list, /l                     - List contexts for current model")
		fmt.Println("  /add <file>,/a                - Stage file for next message (user role)")
		fmt.Println("  /addsystem <file>,/as         - Stage file for system message (system role)")
		fmt.Println("  /r <cmd>                      - Run shell command and show output")
		fmt.Println("  /m <model>                    - Switch model (e.g., /m gpt-4)")
		fmt.Println("  /ms <model-summary            - Switch model used for context summary to comress context")
		fmt.Println("  /url <url>                    - Switch API URL")
		fmt.Println("  /timeout or /t <dur>          - Set request timeout (e.g., 30s, 5m, 1h)")
		fmt.Println("  /msto <number-in-secs>        - Set model summary timeout (e.g., 300 - which 300 secs)")
		fmt.Println("  /exit or /q                   - Exit REPL")
		fmt.Println("  /use <name>                   - Switch to an existing context")
		fmt.Println("  /del <name>                   - Delete specific context")
		fmt.Println("  /del all                      - Delete all contexts for current model")
		fmt.Println("  /debug <0|1|2>                - Enable/Disable debug and set debug level")
		fmt.Println("  /show <thing>                 - Show details (e.g., /show context <name>)")
		fmt.Println("  /showthink <on|off>           - Show thinking process. Default is off")
		fmt.Println("  /cd <dirname>                 - Change to directory")
		fmt.Println("  /mcp <spec>                   - Connect MCP server (tcp://host:port or cmd)")
		fmt.Println("  /mcp off                      - Disconnect MCP server")
		fmt.Println("  /mcp tools                    - List available MCP tools")
		fmt.Println("  /mcp refresh                  - Refresh MCP tool list")
		fmt.Println("  /mcpfunc <func> <perm>        - Set permission for tools func, auto|denied|ask")
		fmt.Println("  /ctx <N>|off                  - Set context token limit (auto-trim when exceeded)")
		fmt.Println("  /trimctx <idx|range>          - Remove messages at index or range (e.g. 3-5, -1--3)")
		fmt.Println("  /configdir <newdir>           - Switch config directory (.aigdotenv and .aig/). If run from from start add prefix dir:// so we dont treat the / as next command. eg. aig /n /configdir dir:///home/user/aig1 /repl - Within a session it is not required")
		fmt.Println()
		fmt.Printf("Curl mode: using curl http://localhost:%d as base for these below cmds\n", statServerPort)
		fmt.Println("  currently only reporting stats")
		fmt.Println("Inline file attachment:")
		fmt.Println("  Include file://<path> anywhere in your message to attach a file inline.")
		fmt.Println("  Example: Summarise this file:/home/user/notes.txt please")

	case "/use":
		if arg == "" {
			fmt.Println("Usage: /use <context-name>")
			return
		}
		sanitizedName := strings.ReplaceAll(arg, " ", "_")
		path := filepath.Join(filepath.Join(homeDir, ".aig"), sanitizedName)
		if _, err := os.Stat(path); os.IsNotExist(err) {
			fmt.Printf("Error: context '%s' not found.\n", arg)
			return
		}
		if err := loadHistory(path, history); err != nil {
			fmt.Printf("Error loading context: %v\n", err)
			return
		}
		currentContextPath = path
		fmt.Printf("✅ Switched to context: %s\n", arg)

	case "/list", "/l":
		fmt.Printf("📜 Contexts for model [%s]:\n", config.Model)
		files, _ := os.ReadDir(filepath.Join(homeDir, ".aig"))
		found := false
		for _, f := range files {
			if !f.IsDir() {
				fmt.Printf("  - %s\n", f.Name())
				found = true
			}
		}
		if !found {
			fmt.Println("  (No contexts found)")
		}
		fmt.Printf(" Current context: %s\n", filepath.Base(currentContextPath))

	case "/del":
		if arg == "" {
			fmt.Println("Usage: /del <name> or /del all")
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
			fmt.Printf("🗑️ Deleted %d contexts.\n", count)
		} else {
			path := filepath.Join(filepath.Join(homeDir, ".aig"), arg+".json")
			if err := os.Remove(path); err != nil {
				fmt.Printf("Error deleting context: %v\n", err)
			} else {
				fmt.Printf("✅ Deleted context: %s\n", arg)
				if currentContextPath != "" && strings.Contains(currentContextPath, arg) {
					currentContextPath = ""
					*history = []Message{}
				}
			}
		}

	case "/debug":
		if arg == "" {
			fmt.Println("Usage: /debug <0|1|2>")
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
			fmt.Println("Usage: /system <text> It will create a new session")
			if len(*history) > 0 {
				fmt.Printf("Current %s prompt: '%s'\n", (*history)[0].Role, (*history)[0].Content)
			}
			return
		}
		systemprompt := strings.Join(parts[1:], " ")
		*history = []Message{
			{Role: "system", Content: systemprompt},
		}
		os.WriteFile(filepath.Join(homeDir, config.Model+".system"), []byte(systemprompt), 0o640)
		fmt.Println("✅ System prompt added")

	case "/add", "/a":
		if arg == "" {
			fmt.Println("Usage: /add <filename>")
			return
		}
		parts, err := processFile(arg)
		if err != nil {
			fmt.Printf("Error processing file %s: %v\n", arg, err)
			return
		}
		pendingFileContent = append(pendingFileContent, parts...)
		fmt.Printf("📎 Staged '%s' — will be included in your next message.\n", arg)
		fmt.Printf("   (Total staged parts: %d)\n", len(pendingFileContent))

	case "/addsystem", "/as":
		if arg == "" {
			fmt.Println("Usage: /addsystem | /as <filename>. Add the file content to the system message")
			return
		}
		parts, err := processFile(arg)
		if err != nil {
			fmt.Printf("Error processing file %s: %v\n", arg, err)
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
			fmt.Println("Usage: /r <command>")
			return
		}
		runSystemCommand(strings.Join(parts[1:], " "))

	case "/m":
		if arg == "" {
			fmt.Println("Usage: /m <model>")
			return
		}
		config.Model = arg
		fmt.Printf("Model switched to: %s\n", arg)

	case "/ms":
		if arg == "" {
			fmt.Println("Usage: /ms <summary-model>")
			return
		}
		config.SummaryModel = arg
		fmt.Printf("Summary Model switched to: %s\n", arg)

	case "/msto":
		if arg == "" {
			fmt.Println("Usage: /msto <summary-model-timout> eg. 300s")
			return
		}
		config.SummaryModelTimeout = arg
		fmt.Printf("Summary Model Timeout switched to: %s\n", arg)

	case "/url":
		if arg == "" {
			fmt.Println("Usage: /url <url>")
			return
		}
		config.BaseURL = arg
		fmt.Printf("URL switched to: %s\n", arg)

	case "/timeout", "/t":
		if arg == "" {
			fmt.Println("Usage: /timeout <duration> (e.g., 30s, 5m, 1h)")
			return
		}
		d, err := time.ParseDuration(arg)
		if err != nil {
			fmt.Printf("Error parsing duration: %v\n", err)
			return
		}
		config.Timeout = d
		fmt.Printf("Timeout set to: %v\n", d)

	case "/p":
		fmt.Printf("Current config: %s\n", u.JsonDump(config, ""))
		if activeMCP != nil {
			fmt.Printf("MCP: %s (%d tools)\n", activeMCP.Spec, len(activeMCP.Tools()))
		} else {
			fmt.Println("MCP: not connected")
		}

	case "/show":
		if arg == "" {
			fmt.Println("Usage: /show <thing> (e.g., /show context <name>)")
			return
		}
		if strings.HasPrefix(arg, "context ") {
			contextName := strings.TrimPrefix(arg, "context ")
			path := filepath.Join(homeDir, ".aig", contextName)
			if _, err := os.Stat(path); os.IsNotExist(err) {
				fmt.Printf("Error: context '%s' not found.\n", contextName)
				return
			}
			var tempHistory []Message
			if err := loadHistory(path, &tempHistory); err != nil {
				fmt.Printf("Error loading context: %v\n", err)
				return
			}
			fmt.Printf("📜 Showing context: %s\n", contextName)
			fmt.Println(strings.Repeat("-", 20))
			for _, msg := range tempHistory {
				switch msg.Role {
				case "user":
					fmt.Printf("user: %s\n", msg.Content)
				case "assistant":
					fmt.Printf("AI: %s\n", msg.Content)
				}
			}
			fmt.Println(strings.Repeat("-", 20))
		} else {
			fmt.Printf("Unknown thing to show: %s. Try '/show context <name>'\n", arg)
		}

	case "/configdir":
		arg = strings.TrimPrefix(arg, "dir://")
		if arg == "" {
			panic("Usage: /configdir <newdir>")
		}
		if currentContextPath != "" {
			saveHistory()
		}
		saveConfig()

		absPath, err := filepath.Abs(arg)
		if err != nil {
			fmt.Printf("❌ Error resolving path: %v\n", err)
			return
		}
		homeDir = absPath
		_ = os.MkdirAll(homeDir, 0755)
		fmt.Println("Re-load config")
		currentContextPath = getLatestContextPath(config.Model)
		config = loadConfig()
		println(u.JsonDump(config, ""))
		historyLoaded = false
		*history = []Message{}

		fmt.Printf("✅ Config directory switched to: %s\n", homeDir)
	default:
		fmt.Printf("Unknown command: %s\n", cmd)
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
			fmt.Printf("Error: %v\n", err)
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
