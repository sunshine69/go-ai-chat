package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/chzyer/readline"
	"github.com/joho/godotenv"
	u "github.com/sunshine69/golang-tools/utils"
)

type Config struct {
	BaseURL        string
	Model          string
	APIKey         string
	Timeout        time.Duration
	PromptedURL    bool
	PromptedModel  bool
	PromptedAPIKey bool
	Debug          bool
}

type Message struct {
	Role     string      `json:"role"`
	Content  interface{} `json:"content"`
	Thinking string      `json:"thinking,omitempty"`
}

// ContentPart is used for multimodal messages (OpenAI/Anthropic standard)
type ContentPart struct {
	Type     string    `json:"type"`
	Text     string    `json:"text,omitempty"`
	ImageURL *ImageURL `json:"image_url,omitempty"`
}

type ImageURL struct {
	URL string `json:"url"`
}

type FileProcessor interface {
	Process(path string) (interface{}, error)
}

type TextProcessor struct{}

func (p *TextProcessor) Process(path string) (interface{}, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	// Wrap in markdown for context
	return fmt.Sprintf("Reference File Content (%s):\n```\n%s\n```\n", filepath.Base(path), string(content)), nil
}

type ImageProcessor struct{}

func (p *ImageProcessor) Process(path string) (interface{}, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	mimeType := mime.TypeByExtension(filepath.Ext(path))
	if mimeType == "" {
		mimeType = "image/jpeg" // Fallback
	}

	base64Data := base64.StdEncoding.EncodeToString(content)
	dataURL := fmt.Sprintf("data:%s;base64,%s", mimeType, base64Data)

	// Multimodal structure: text instruction + image
	return []ContentPart{
		{
			Type: "text",
			Text: fmt.Sprintf("Attached image: %s", filepath.Base(path)),
		},
		{
			Type: "image_url",
			ImageURL: &ImageURL{
				URL: dataURL,
			},
		},
	}, nil
}

func getFileProcessor(path string) FileProcessor {
	ext := filepath.Ext(path)
	switch ext {
	case ".png", ".jpg", ".jpeg", ".gif", ".webp":
		return &ImageProcessor{}
	default:
		return &TextProcessor{}
	}
}

func processFile(path string) (interface{}, error) {
	return getFileProcessor(path).Process(path)
}

type Response struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	Choices []struct {
		Delta struct {
			ReasoningContent string `json:"reasoning_content,omitempty"`
			Content          string `json:"content"`
		} `json:"delta"`
		Message struct {
			Content string `json:"content"`
			Role    string `json:"role"`
		} `json:"message"`
	} `json:"choices"`
}

type History struct {
	History []Message `json:"history"`
}

var (
	history            []Message
	historyLoaded      bool = false
	homeDir, _              = os.UserHomeDir()
	config             *Config
	currentContextPath string // Track the currently active context file
)

func saveHistoryToFile(history []Message, index int, filename string) error {

	if index <= 0 || index > len(history) {
		return fmt.Errorf("history empty or index is not > len of history")
	}
	msg := history[index-1]
	switch v := msg.Content.(type) {
	case string:
		content := v
		return os.WriteFile(filename, []byte(content), 0o644)
	default:
		fmt.Println("[INFO] skip saving non text content")
	}
	return nil
}

// generateContextName creates a safe filename: YYYYMMDD-HHMMSS_<trunc(first_user_question)_30>_<model>.json
func generateContextName(model string, firstQuestion string) string {
	// Clean model name: replace spaces with underscores
	cleanModel := strings.ReplaceAll(model, " ", "_")

	// Truncate first question to 30 chars, replace unsafe chars
	safeContent := strings.ReplaceAll(firstQuestion, " ", "_")
	if len(safeContent) > 30 {
		safeContent = safeContent[:30]
	}
	// Further sanitize: only keep alphanumeric, _, -
	sanitized := make([]rune, 0, len(safeContent))
	for _, r := range safeContent {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '-' {
			sanitized = append(sanitized, r)
		} else {
			sanitized = append(sanitized, '_') // fallback
		}
	}

	timestamp := time.Now().Format("20060102-150405")
	return fmt.Sprintf("%s_%s_%s.json", timestamp, string(sanitized), cleanModel)
}

// saveHistory saves the current history to the currentContextPath
func saveHistory() error {
	if currentContextPath == "" {
		return fmt.Errorf("no context path set — cannot save history")
	}

	dir := filepath.Dir(currentContextPath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}

	h := History{History: history}
	data, err := json.MarshalIndent(h, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(currentContextPath, data, 0600)
}

// getLatestContextPath finds the newest context file for the current model in ~/.aig/
func getLatestContextPath(model string) string {
	dir := filepath.Join(homeDir, ".aig")
	files, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}

	var latestPath string
	var latestTime int64

	for _, f := range files {
		if f.IsDir() || !strings.HasSuffix(f.Name(), "_"+model+".json") {
			continue
		}
		// Extract timestamp portion from filename: YYYYMMDD-HHMMSS
		parts := strings.Split(f.Name(), "_")
		if len(parts) < 2 {
			continue
		}
		tsStr := parts[0] // YYYYMMDD-HHMMSS
		if t, err := time.Parse("20060102-150405", tsStr); err == nil {
			if t.Unix() > latestTime {
				latestTime = t.Unix()
				latestPath = filepath.Join(dir, f.Name())
			}
		}
	}
	return latestPath
}

func printHistory(history []Message) {
	fmt.Println("✅ Chat History (Current Session):")
	for i, msg := range history {
		role := "User"
		if msg.Role == "assistant" {
			role = "AI"
		}

		var displayContent string
		switch v := msg.Content.(type) {
		case string:
			displayContent = v
		case []ContentPart:
			displayContent = "[Multimodal Content]"
		default:
			displayContent = v.(string)
		}

		if msg.Thinking != "" {
			thinkPreview := msg.Thinking
			// Replace newlines with spaces to prevent multi-line expansion in the preview
			thinkPreview = strings.ReplaceAll(thinkPreview, "\n", " ")

			// Truncate to ~147 visible characters max
			if utf8.RuneCountInString(thinkPreview) > 147 {
				runes := []rune(thinkPreview)
				thinkPreview = string(runes[:144]) + "..."
			}
			fmt.Printf(" %d [%s]: 🤔 %s\n   💬 %s\n", i+1, role, thinkPreview, displayContent)
		} else if utf8.RuneCountInString(displayContent) > 200 {
			// Fallback: if Thinking is empty but response is long, truncate response
			runes := []rune(displayContent)
			displayContent = string(runes[:197]) + "..."
			fmt.Printf(" %d [%s]: 💬 %s\n", i+1, role, displayContent)
		} else {
			// Short response, no thinking
			fmt.Printf(" %d [%s]: 💬 %s\n", i+1, role, displayContent)
		}
	}
}

func main() {
	_ = godotenv.Load()
	homeEnv, _ := godotenv.Read(homeDir + "/.aigdotenv")

	// Merge or prefer home dir env vars over current dir
	for k, v := range homeEnv {
		if os.Getenv(k) == "" {
			_ = os.Setenv(k, v)
		}
	}

	config = loadConfig()

	// Setup context directory
	aigDir := filepath.Join(homeDir, ".aig")
	_ = os.MkdirAll(aigDir, 0755)

	// Load latest context file if exists
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

	// --- REPL Mode ---
	fmt.Println("AI Chat CLI - REPL Mode")
	fmt.Println("Commands: /new, /add <file>, /r <cmd>, /exit, /history, /m <model>, /url <url> /list, /del <context>, /use <context>, /timeout <dur>")
	fmt.Println("Use ↑/↓ arrow keys to navigate previous messages; type /history to see all.")
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

	runREPLWithReader(config, &history, rl, histFile)

	// Save final state (if we had an active context)
	saveConfig()
	if currentContextPath != "" {
		if err := saveHistory(); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: Could not save history: %v\n", err)
		}
	}
}

// --- Non-Interactive Argument Parsing ---

func handleNonInteractive(config Config) {
	args := os.Args[1:]
	i := 0

	for i < len(args) {
		arg := args[i]

		if arg == "/add" {
			if i+1 >= len(args) {
				fmt.Println("Error: /add requires a filename")
				os.Exit(1)
			}
			i++
			filePath := args[i]
			content, err := os.ReadFile(filePath)
			if err != nil {
				fmt.Printf("Error reading file %s: %v\n", filePath, err)
				os.Exit(1)
			}
			history = append([]Message{
				{Role: "system", Content: fmt.Sprintf("Reference File Content:\n```\n%s\n```\n", string(content))},
			}, history...)
			i++
			continue
		}

		if arg == "/new" {
			if i+1 < len(args) && !strings.HasPrefix(args[i+1], "/") {
				// Consume the next arg as the initial message
				message := args[i+1]
				i += 2
				history = []Message{{Role: "user", Content: message}}
				continue
			}
			// If no message provided, just clear history
			history = []Message{}
			i++
			continue
		}

		if arg == "/repl" {
			runREPL(config)
			return
		}

		if arg == "/r" {
			if i+1 >= len(args) {
				fmt.Println("Error: /r requires a command")
				os.Exit(1)
			}
			i++
			runSystemCommand(args[i])
			i++
			continue
		}

		if arg == "/m" {
			if i+1 >= len(args) {
				fmt.Println("Error: /m requires a model name")
				os.Exit(1)
			}
			i += 2
			continue
		}

		if arg == "/url" {
			if i+1 >= len(args) {
				fmt.Println("Error: /url requires a URL")
				os.Exit(1)
			}
			i += 2
			continue
		}

		if !strings.HasPrefix(arg, "/") {
			// Single shot execution: treat as a direct prompt
			question := strings.Join(args[i:], " ")
			history = append(history, Message{Role: "user", Content: question})
			ctx, cancel := context.WithCancel(context.Background())
			sigChan := make(chan os.Signal, 1)
			signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)

			go func() {
				<-sigChan
				fmt.Print("\n⏹️  Interrupted. Stopping response...\n")
				cancel()
			}()

			ans, _, err := askAI(ctx, config, history)
			if err != nil {
				fmt.Fprintf(os.Stderr, "Error: %v\n", err)
				os.Exit(1)
			}
			fmt.Print(ans)
			return
		}

		// Skip unknown commands or flags
		fmt.Fprintf(os.Stderr, "Warning: Unknown command or unexpected argument: %s\n", arg)
		i++
	}

	// Default fallback: if we parsed some commands but didn't answer yet, fall back to /repl
	runREPL(config)
}

func runREPL(config Config) {
	histFile := filepath.Join(homeDir, ".aig_history_lines")
	rl, err := readline.NewEx(&readline.Config{Prompt: "> ", HistoryFile: histFile, HistoryLimit: 5000})
	if err != nil {
		rl = nil
	}
	cfgPtr := config
	runREPLWithReader(&cfgPtr, &history, rl, histFile)
}

// --- Helper Functions ---

func runSystemCommand(cmdStr string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Sanitize command to prevent command injection
	if strings.Contains(cmdStr, "&&") || strings.Contains(cmdStr, "||") || strings.Contains(cmdStr, ";") {
		fmt.Println("⚠️  Command contains forbidden operators (&&, ||, or ;)")
		return
	}

	cmd := exec.CommandContext(ctx, "sh", "-c", cmdStr)
	o, err := u.RunSystemCommandV3(cmd, true)

	if ctx.Err() == context.DeadlineExceeded {
		fmt.Println("⚠️ Command timed out after 30s")
		return
	}

	if err != nil {
		fmt.Printf("❌ Command failed: %v\nStderr: %s\n", err, o)
		return
	}
	fmt.Printf("✅ Output:\n%s\n", o)
}

func askAI(ctx context.Context, config Config, history []Message) (string, string, error) {
	url := config.BaseURL
	if !strings.Contains(url, "/chat/completions") {
		url = strings.TrimSuffix(url, "/") + "/chat/completions"
	}

	jsonData := map[string]interface{}{
		"model":    config.Model,
		"messages": history,
		"stream":   true,
	}

	jsonValue, _ := json.Marshal(jsonData)
	client := &http.Client{Timeout: config.Timeout}

	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewBuffer(jsonValue))
	if err != nil {
		return "", "", err
	}

	req.Header.Set("Content-Type", "application/json")
	if config.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+config.APIKey)
	}

	resp, err := client.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		return "", "", fmt.Errorf("API Error: %s", string(body))
	}

	scanner := bufio.NewScanner(resp.Body)
	var fullContent strings.Builder
	var thinkingContent strings.Builder
	var thinkingStarted = false
	var headerPrinted = false

	// ✅ Immediately unblock scanner when context is cancelled
	go func() {
		<-ctx.Done()
		resp.Body.Close()
	}()

	for scanner.Scan() {
		if ctx.Err() != nil {
			break // ✅ Exit loop instantly on cancellation
		}

		line := strings.TrimSpace(scanner.Text())
		if line == "" || line == "data: [DONE]" || !strings.HasPrefix(line, "data: ") {
			continue
		}

		streamData := strings.TrimPrefix(line, "data: ")
		var streamResp Response
		if err := json.Unmarshal([]byte(streamData), &streamResp); err != nil {
			continue
		}

		if len(streamResp.Choices) > 0 {
			delta := streamResp.Choices[0].Delta

			// 🤔 Stream thinking content
			if delta.ReasoningContent != "" {
				if !thinkingStarted {
					fmt.Print("\n> 🤔 Thinking...\n")
					thinkingStarted = true
				}
				fmt.Print(delta.ReasoningContent)
				os.Stdout.Sync()
				thinkingContent.WriteString(delta.ReasoningContent)
			}

			// 📝 Stream main response
			if delta.Content != "" {
				if !headerPrinted {
					fmt.Print("\n> 📝 Response:\n")
					headerPrinted = true // ✅ Prints exactly once
				}
				fmt.Print(delta.Content)
				os.Stdout.Sync()
				fullContent.WriteString(delta.Content)
			}
		}
	}

	if err := scanner.Err(); err != nil && ctx.Err() == nil {
		return fullContent.String(), thinkingContent.String(), fmt.Errorf("stream error: %v", err)
	}

	return fullContent.String(), thinkingContent.String(), nil
}

// --- File Management ---

func getHistoryFilePath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ".aihistory.json"
	}
	return filepath.Join(home, ".aihistory.json")
}

// loadHistory loads a context file into memory (history)
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
	// Only append non-empty lines that are not commands
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

// --- Config Management ---

func saveConfig() {
	if config == nil {
		return
	}

	dotEnvPath := filepath.Join(homeDir, ".aigdotenv")

	// Read existing .aigdotenv (if exists)
	envVars := make(map[string]string)
	if data, err := os.ReadFile(dotEnvPath); err == nil {
		lines := strings.Split(string(data), "\n")
		for _, line := range lines {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			parts := strings.SplitN(line, "=", 2)
			if len(parts) == 2 {
				envVars[parts[0]] = parts[1]
			}
		}
	}

	// Update env vars with current config values (even if not flagged as "prompted")
	// But only if they differ from defaults (and not empty)
	changed := false
	if config.BaseURL != "" && config.BaseURL != "https://api.openai.com/v1/chat/completions" {
		envVars["OPENAI_URL"] = config.BaseURL
		changed = true
	}
	if config.Model != "" && config.Model != "gpt-3.5-turbo" {
		envVars["OPENAI_MODEL"] = config.Model
		changed = true
	}
	if config.APIKey != "" {
		envVars["OPENAI_API_KEY"] = config.APIKey
		changed = true
	}
	if config.Timeout != 45*time.Minute {
		envVars["TIMEOUT"] = config.Timeout.String()
		changed = true
	}

	if !changed {
		return // no meaningful changes
	}

	// Write back to .aigdotenv
	var sb strings.Builder
	for k, v := range envVars {
		sb.WriteString(fmt.Sprintf("%s=%s\n", k, v))
	}

	if err := os.WriteFile(dotEnvPath, []byte(sb.String()), 0600); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: Could not save config to %s: %v\n", dotEnvPath, err)
	} else {
		fmt.Println("✅ Configuration saved to ~/.aigdotenv")
	}
}

func promptForMissingConfig(config *Config) {
	reader := bufio.NewReader(os.Stdin)

	// Check which values are missing
	needsURL := config.BaseURL == ""
	needsModel := config.Model == ""
	needsAPIKey := config.APIKey == ""

	if !needsURL && !needsModel && !needsAPIKey {
		return // No prompting needed
	}

	fmt.Println()
	fmt.Println("🤖 AI Chat CLI needs configuration")
	fmt.Println("We'll help you set up your OpenAI API credentials.")
	fmt.Println()

	// Prompt for URL if missing
	if needsURL {
		fmt.Printf("API URL [%s]: ", config.BaseURL)
		if input, _ := reader.ReadString('\n'); strings.TrimSpace(input) != "" {
			config.BaseURL = strings.TrimSpace(input)
			config.PromptedURL = true
		}
	}

	// Prompt for Model if missing
	if needsModel {
		fmt.Printf("Model [%s]: ", config.Model)
		if input, _ := reader.ReadString('\n'); strings.TrimSpace(input) != "" {
			config.Model = strings.TrimSpace(input)
			config.PromptedModel = true
		}
	}

	// Prompt for API Key if missing
	if needsAPIKey {
		fmt.Printf("API Key (will not be displayed): ")
		apiKey, _ := reader.ReadString('\n')
		apiKey = strings.TrimSpace(string(apiKey))
		if apiKey != "" {
			config.APIKey = apiKey
			config.PromptedAPIKey = true
		}
	}

	// Save config to .aigdotenv
	fmt.Println()
	if needsURL || needsModel || needsAPIKey {
		saveConfig()
		fmt.Println("✅ Configuration saved to ~/.aigdotenv")
		fmt.Println()
	}
}

func loadConfig() *Config {
	c := Config{
		BaseURL:        os.Getenv("OPENAI_URL"),
		Model:          os.Getenv("OPENAI_MODEL"),
		APIKey:         os.Getenv("OPENAI_API_KEY"),
		Timeout:        45 * time.Minute,
		PromptedURL:    false,
		PromptedModel:  false,
		PromptedAPIKey: false,
	}

	if timeoutStr := os.Getenv("TIMEOUT"); timeoutStr != "" {
		if d, err := time.ParseDuration(timeoutStr); err == nil {
			c.Timeout = d
		} else if seconds, err := strconv.Atoi(timeoutStr); err == nil {
			c.Timeout = time.Duration(seconds) * time.Second
		}
	}

	if c.BaseURL == "" {
		c.BaseURL = "https://api.openai.com/v1/chat/completions"
	}
	if c.Model == "" {
		c.Model = "gpt-3.5-turbo"
	}

	if strings.Contains(c.BaseURL, "localhost:11434") || strings.Contains(c.BaseURL, "localhost:4333") {
		if c.Model == "" {
			c.Model = "llama3"
		}
	}

	// Prompt for missing values if in REPL mode (not in non-interactive mode)
	if len(os.Args) == 1 {
		promptForMissingConfig(&c)
	}

	return &c
}

// Marker
func handleCommand(text string, config *Config, history *[]Message) {
	parts := strings.SplitN(text, " ", 2)
	cmd := parts[0]
	arg := ""
	if len(parts) > 1 {
		arg = parts[1]
	}

	switch cmd {

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

		// Save only up to idx
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
		// Before resetting, save current session if history not empty
		if len(*history) > 0 {
			// Generate name: use first user message as base
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
			newContextName := generateContextName(config.Model, firstUserMsg)
			newContextPath := filepath.Join(homeDir, ".aig", newContextName)

			// Save current session to new file
			if err := saveHistory(); err != nil {
				fmt.Printf("⚠️  Could not save current session: %v\n", err)
			} else {
				fmt.Printf("✅ Saved current session to: %s\n", filepath.Base(newContextPath))
			}
		}

		// Now reset history & context path
		*history = []Message{}
		// Generate fresh context name (will be set in first user question save)
		currentContextPath = ""
		fmt.Println("✅ New context started.")

	case "/help":
		fmt.Println("Commands:")
		fmt.Println("  /new , /n                     - Clear conversation history")
		fmt.Println("  /add <file>,/a                - Add file contents to context")
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
		fmt.Println("  /s <hist_index:filename>      - Save the history index to a file ")
		fmt.Println("  /cd <dirname>                 - Change to directory")

	case "/use":
		if arg == "" {
			fmt.Println("Usage: /use <context-name>")
			return
		}
		sanitizedName := strings.ReplaceAll(arg, " ", "_")
		path := filepath.Join(filepath.Join(homeDir, ".aig"), sanitizedName+".json")

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
			if !f.IsDir() && strings.HasSuffix(f.Name(), "_"+config.Model+".json") {
				fmt.Printf("  - %s\n", f.Name())
				found = true
			}
		}
		if !found {
			fmt.Println("  (No contexts found)")
		}
		fmt.Printf(" Current context: %s\n", currentContextPath)

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
		if arg == "1" {
			config.Debug = true
		} else {
			config.Debug = false
		}

	case "/system", "/sys":
		if arg == "" {
			fmt.Println("Usage: /system <text>")
			return
		}
		*history = append([]Message{
			{Role: "system", Content: arg},
		}, *history...)
		fmt.Printf("✅ System prompt added: %s\n", arg)

	case "/add", "/a":
		if arg == "" {
			fmt.Println("Usage: /add <filename>")
			return
		}
		content, err := processFile(arg)
		if err != nil {
			fmt.Printf("Error processing file %s: %v\n", arg, err)
			return
		}
		*history = append([]Message{
			{Role: "system", Content: content},
		}, *history...)

		if config.Debug {
			fmt.Printf("Added '%s' to conversation context.\n", arg)
		}

	case "/r":
		if arg == "" {
			fmt.Println("Usage: /r <command>")
			return
		}
		runSystemCommand(arg)

	case "/m":
		if arg == "" {
			fmt.Println("Usage: /m <model>")
			return
		}
		config.Model = arg
		fmt.Printf("Model switched to: %s\n", arg)

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

	case "/p": // print current config
		fmt.Printf("Current config: %s\n", u.JsonDump(config, ""))

	case "/show":
		if arg == "" {
			fmt.Println("Usage: /show <thing> (e.g., /show context <name>)")
			return
		}
		if strings.HasPrefix(arg, "context ") {
			contextName := strings.TrimPrefix(arg, "context ")
			path := filepath.Join(homeDir, ".aig", contextName+".json")

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

// --- REPL logic with save hooks ---

func runREPLWithReader(config *Config, history *[]Message, rl *readline.Instance, histFile string) {
	printPrompt := func() { fmt.Print("You: ") }
	printOutput := func(s string) { fmt.Printf("AI: %s\n", s) }

	if rl == nil {
		// Fallback to basic scanner input
		scanner := bufio.NewScanner(os.Stdin)
		for {
			printPrompt()
			if !scanner.Scan() {
				fmt.Println()
				break
			}
			text := strings.TrimSpace(scanner.Text())
			if text == "" {
				continue
			}
			if strings.HasPrefix(text, "/") {
				if text == "/exit" || text == "/q" {
					return
				}
				if strings.HasPrefix(text, "/h") {
					printHistory(*history)
					continue
				}
				handleCommand(text, config, history)
				if config.Debug {
					fmt.Printf("[DEBUG] config: %v\n", *config)
				}
				continue
			}

			// Save user input to readline history file (only non-command lines)
			if err := appendHistoryFile(histFile, text); err != nil {
				fmt.Fprintf(os.Stderr, "Warning: Could not save line to history file: %v\n", err)
			}

			// If no context path yet and first user message, set context path
			if currentContextPath == "" {
				firstUserMsg := text
				newContextName := generateContextName(config.Model, firstUserMsg)
				currentContextPath = filepath.Join(homeDir, ".aig", newContextName)
			}

			*history = append(*history, Message{Role: "user", Content: text})

			ctx, cancel := context.WithCancel(context.Background())
			sigChan := make(chan os.Signal, 1)
			signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)

			go func() {
				<-sigChan
				fmt.Print("\n⏹️  Interrupted. Stopping response...\n")
				cancel()
			}()

			ans, thinking, err := askAI(ctx, *config, *history)
			if err != nil {
				fmt.Printf("Error: %v\n", err)
				*history = (*history)[:len(*history)-1]
				continue
			}

			if ctx.Err() != nil {
				continue
			}

			printOutput(ans)
			*history = append(*history, Message{
				Role:     "assistant",
				Content:  ans,
				Thinking: thinking,
			})

			// Save session to current context file after each turn
			if err := saveHistory(); err != nil {
				fmt.Fprintf(os.Stderr, "⚠️  Could not save history: %v\n", err)
			}
		}
		return
	}

	defer rl.Close()

	for {
		line, err := rl.Readline()
		if err != nil {
			fmt.Println()
			break
		}

		text := strings.TrimSpace(line)
		if text == "" {
			continue
		}

		if strings.HasPrefix(text, "/") {
			if text == "/exit" || text == "/q" {
				return
			}
			if strings.HasPrefix(text, "/h") {
				printHistory(*history)
				continue
			}
			handleCommand(text, config, history)
			if config.Debug {
				fmt.Printf("[DEBUG] config: %v\n", *config)
			}
			continue
		}

		// Save user input to readline history file (only non-command lines)
		if err := appendHistoryFile(histFile, text); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: Could not save line to history file: %v\n", err)
		}

		// If no context path yet and first user message, set context path
		if currentContextPath == "" {
			firstUserMsg := text
			newContextName := generateContextName(config.Model, firstUserMsg)
			currentContextPath = filepath.Join(homeDir, ".aig", newContextName)
		}

		*history = append(*history, Message{Role: "user", Content: text})

		ctx, cancel := context.WithCancel(context.Background())
		sigChan := make(chan os.Signal, 1)
		signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)

		go func() {
			<-sigChan
			fmt.Print("\n⏹️  Interrupted. Stopping response...\n")
			cancel()
		}()

		ans, thinking, err := askAI(ctx, *config, *history)
		if err != nil {
			fmt.Printf("Error: %v\n", err)
			*history = (*history)[:len(*history)-1]
			continue
		}

		if ctx.Err() != nil {
			continue
		}

		printOutput(ans)
		*history = append(*history, Message{
			Role:     "assistant",
			Content:  ans,
			Thinking: thinking,
		})

		// Save session to current context file after each turn
		if err := saveHistory(); err != nil {
			fmt.Fprintf(os.Stderr, "⚠️  Could not save history: %v\n", err)
		}
	}
}
