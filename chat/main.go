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

	"github.com/chzyer/readline"
	"github.com/joho/godotenv"
	u "github.com/sunshine69/golang-tools/utils"
)

// 1. Change pendingFileContent from string to []ContentPart
var pendingFileContent []ContentPart // was: var pendingFileContent string

// ---------------------------------------------------------------------------
// extractInlineFiles — now returns []ContentPart for file content
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
				kept = append(kept, w) // leave token so the user sees it
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
	// Collect all parts in order: staged files → inline files → user text
	var allParts []ContentPart

	if len(pendingFileContent) > 0 {
		allParts = append(allParts, pendingFileContent...)
		pendingFileContent = nil // clear staged content
	}

	allParts = append(allParts, inlineParts...)

	// Always append the user's text as the final part
	if text != "" {
		allParts = append(allParts, ContentPart{Type: "text", Text: text})
	}

	// If everything is plain text parts, collapse to a single string so the
	// conversation history stays simple for text-only exchanges.
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
func main() {
	_ = godotenv.Load()
	homeEnv, _ := godotenv.Read(homeDir + "/.aigdotenv")
	for k, v := range homeEnv {
		if os.Getenv(k) == "" {
			_ = os.Setenv(k, v)
		}
	}

	config = loadConfig()

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
		fmt.Println("Use ↑/↓ arrow keys to navigate previous messages; type /history to see all.")
		fmt.Println("Inline file attachment: include file://<path> anywhere in your message.")
		fmt.Println("----------------------------------------")
		if historyLoaded {
			fmt.Printf("Loaded %d messages from previous session.\n", len(history)/2)
			fmt.Println("Type /new to start fresh if desired.")
			fmt.Println()
		}

		histFile := filepath.Join(homeDir, ".aig_history_lines")
		rl, err := readline.NewEx(&readline.Config{Prompt: "> ", HistoryFile: histFile, HistoryLimit: 5000})
		if err != nil {
			rl = nil
		}
		runREPLWithReader(&history, rl, histFile)
	} else {
		// Non-Interactive Mode (One-shot commands or chat)
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

		// A command must start with "/" but NOT be a relative path like "./foo" or "../foo"
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
			runREPL()
			runmode = ""
			return

		case "/c", "/chat", "/q", "/question":
			// Collect the rest of the args as the question
			if i >= len(args) {
				fmt.Fprintln(os.Stderr, "❌ No question provided after", cmd)
				return
			}
			rawQuestion := strings.Join(args[i:], " ")
			i = len(args) // consume all remaining args

			// Resolve inline file:// references in the question
			question, inlineContent := extractInlineFiles(rawQuestion)

			ctx, cancel := context.WithCancel(context.Background())
			sigChan := make(chan os.Signal, 1)
			signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)
			go func() {
				<-sigChan
				fmt.Print("\n⏹️  Interrupted. Stopping response...\n")
				cancel()
			}()

			// Set context path if not already set
			if currentContextPath == "" {
				newContextName := generateContextName(config.Model, question)
				currentContextPath = filepath.Join(homeDir, ".aig", newContextName)
			}

			// Build user message: staged files + inline files + question text
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
			// Generic command: collect args until the next slash-command
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

func runREPL() {
	histFile := filepath.Join(homeDir, ".aig_history_lines")
	rl, err := readline.NewEx(&readline.Config{Prompt: "> ", HistoryFile: histFile, HistoryLimit: 5000})
	if err != nil {
		rl = nil
	}
	runREPLWithReader(&history, rl, histFile)
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

		// Convert negative indices to absolute positions
		absStart := start
		if start < 0 {
			absStart = hLen + start
		}
		absEnd := end
		if end < 0 {
			absEnd = hLen + end
		}

		// Determine the actual boundaries (handles cases like 5-3 or -1--3)
		minIdx, maxIdx := absStart, absEnd
		if absStart > absEnd {
			minIdx, maxIdx = absEnd, absStart
		}

		// Validate bounds
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
			fmt.Println("Usage: /mcpfunc <name> <auto|ask|deny>")
			return
		}
		if len(parts) < 3 {
			fmt.Println("❌ Error: Missing permission level. Usage: /mcpfunc <name> <auto|ask|deny>")
			return
		}

		toolName := parts[1]
		perm := strings.ToLower(parts[2])

		if perm != "auto" && perm != "ask" && perm != "deny" {
			fmt.Println("❌ Error: Invalid permission level. Use 'auto', 'ask', or 'deny'.")
			return
		}

		config.MCPPermissions[toolName] = perm
		saveConfig()
		fmt.Printf("✅ Permission for '%s' set to [%s]\n", toolName, perm)

	case "/edit":
		fmt.Println("📝 Opening editor... Write your text, save, and exit to send.")
		return

	// -----------------------------------------------------------------------
	// MCP commands
	// -----------------------------------------------------------------------

	case "/mcp":
		if arg == "" {
			// Show current MCP status
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

		// No sub-command matched — treat the rest as MCP server spec to connect
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

	// -----------------------------------------------------------------------
	// Original commands
	// -----------------------------------------------------------------------

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
		// 1. Save the OLD history if it exists before clearing
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

		// 2. Reset the history for the new session
		*history = []Message{}
		pendingFileContent = nil // also clear any staged file

		// 3. Handle the NEW context naming
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
		fmt.Println("  /add <file>,/a                - Stage file for next message (user role)")
		fmt.Println("  /addsystem <file>,/as         - Stage file for system message (system role)")
		fmt.Println("  /r <cmd>                      - Run shell command and show output")
		fmt.Println("  /m <model>                    - Switch model (e.g., /m gpt-4)")
		fmt.Println("  /url <url>                    - Switch API URL")
		fmt.Println("  /timeout or /t <dur>          - Set request timeout (e.g., 30s, 5m, 1h)")
		fmt.Println("  /exit or /q                   - Exit REPL")
		fmt.Println("  /history or /h                - Show current chat history")
		fmt.Println("  /list, /l                     - List contexts for current model")
		fmt.Println("  /use <name>                   - Switch to an existing context")
		fmt.Println("  /del <name>                   - Delete specific context")
		fmt.Println("  /del all                      - Delete all contexts for current model")
		fmt.Println("  /debug <0|1>                  - Enable/Disable debug")
		fmt.Println("  /show <thing>                 - Show details (e.g., /show context <name>)")
		fmt.Println("  /s <hist_index:filename>      - Save the history index to a file")
		fmt.Println("  /cd <dirname>                 - Change to directory")
		fmt.Println("  /mcp <spec>                   - Connect MCP server (tcp://host:port or cmd)")
		fmt.Println("  /mcp off                      - Disconnect MCP server")
		fmt.Println("  /mcp tools                    - List available MCP tools")
		fmt.Println("  /mcp refresh                  - Refresh MCP tool list")
		fmt.Println("  /mcpfunc <func> <perm>        - Set permission for tools func, auto|denied|ask")
		fmt.Println("  /ctx <N>|off                  - Set context token limit (auto-trim when exceeded)")
		fmt.Println("  /trimctx <idx|range>          - Remove messages at index or range (e.g. 3-5, -1--3)")
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
			fmt.Println("Usage: /debug <0|1>")
			return
		}
		config.Debug = arg == "1"

	case "/system", "/sys":
		if arg == "" {
			fmt.Println("Usage: /system <text> It will create a new session")
			if len(*history) > 0 {
				fmt.Printf("Current %s prompt: '%s'\n", (*history)[0].Role, (*history)[0].Content)
			}
			return
		}
		*history = []Message{
			{Role: "system", Content: strings.Join(parts[1:], " ")},
		}
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

	// ---------------------------------------------------------------------------
	// /addsystem handler — wrap []ContentPart into a system message
	// ---------------------------------------------------------------------------

	// Inside handleCommand, replace the "/addsystem" case:
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
		// System messages are text-only in practice; collapse if possible.
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

	default:
		fmt.Printf("Unknown command: %s\n", cmd)
	}
}

// ---------------------------------------------------------------------------
// REPL loop
// ---------------------------------------------------------------------------

func runREPLWithReader(history *[]Message, rl *readline.Instance, histFile string) {
	printPrompt := func() { fmt.Print("You: ") }

	doTurn := func(ctx context.Context, text string) {
		// Resolve inline file:// references
		cleanText, inlineContent := extractInlineFiles(text)

		// Build final user message (staged files + inline files + text)
		userMsg := buildUserMessage(cleanText, inlineContent)

		// Ensure context path is set
		if currentContextPath == "" {
			newContextName := generateContextName(config.Model, cleanText)
			currentContextPath = filepath.Join(homeDir, ".aig", newContextName)
		}

		*history = append(*history, Message{Role: "user", Content: userMsg})
		ans, thinking, l_history, err := askAI(ctx, *config, *history)
		if err != nil {
			fmt.Printf("Error: %v\n", err)
			*history = (l_history)[:len(l_history)-1]
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

	readLine := func() (string, bool) {
		if rl != nil {
			line, err := rl.Readline()
			if err != nil {
				return "", false
			}
			return strings.TrimSpace(line), true
		}
		printPrompt()
		scanner := bufio.NewScanner(os.Stdin)
		if !scanner.Scan() {
			return "", false
		}
		return strings.TrimSpace(scanner.Text()), true
	}

	if rl != nil {
		defer rl.Close()
	}

	for {
		text, ok := readLine()
		if !ok {
			fmt.Println()
			break
		}
		if text == "" {
			continue
		}

		if strings.HasPrefix(text, "/") {
			if text == "/edit" {
				editorText, err := openInEditor("")
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
				go func() {
					<-sigChan
					cancel()
				}()

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

		// Save to readline history
		if err := appendHistoryFile(histFile, text); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: Could not save line to history file: %v\n", err)
		}

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

// openInEditor opens the user's default terminal editor with the provided
// initial text, waits for it to exit, and returns the saved content.
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
