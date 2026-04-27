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

// https://github.com/hymkor/go-multiline-ny for multiline support TODO
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
	Role       string      `json:"role"`
	Content    interface{} `json:"content"`
	Thinking   string      `json:"thinking,omitempty"`
	ToolCallID string      `json:"tool_call_id,omitempty"`
	ToolCalls  []ToolCall  `json:"tool_calls,omitempty"`
	Name       string      `json:"name,omitempty"`
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
		mimeType = "image/jpeg"
	}

	base64Data := base64.StdEncoding.EncodeToString(content)
	dataURL := fmt.Sprintf("data:%s;base64,%s", mimeType, base64Data)

	return []ContentPart{
		{Type: "text", Text: fmt.Sprintf("Attached image: %s", filepath.Base(path))},
		{Type: "image_url", ImageURL: &ImageURL{URL: dataURL}},
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

// ---------------------------------------------------------------------------
// Streaming response types (OpenAI SSE)
// ---------------------------------------------------------------------------

type Response struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	Choices []struct {
		Delta struct {
			ReasoningContent string     `json:"reasoning_content,omitempty"`
			Content          string     `json:"content"`
			ToolCalls        []ToolCall `json:"tool_calls,omitempty"`
		} `json:"delta"`
		FinishReason string `json:"finish_reason"`
		Message      struct {
			Content   string     `json:"content"`
			Role      string     `json:"role"`
			ToolCalls []ToolCall `json:"tool_calls,omitempty"`
		} `json:"message"`
	} `json:"choices"`
}

// ---------------------------------------------------------------------------
// History / context persistence
// ---------------------------------------------------------------------------

type History struct {
	History []Message `json:"history"`
}

var (
	history            []Message
	historyLoaded      bool = false
	homeDir, _              = os.UserHomeDir()
	config             *Config
	currentContextPath string

	// Global MCP client (single active connection)
	activeMCP *MCPClient
)

func saveHistoryToFile(history []Message, index int, filename string) error {
	if index <= 0 || index > len(history) {
		return fmt.Errorf("history empty or index is not > len of history")
	}
	msg := history[index-1]
	switch v := msg.Content.(type) {
	case string:
		if err := os.WriteFile(filename, []byte(v), 0o644); err != nil {
			return err
		}
	default:
		fmt.Println("[INFO] skip saving non text content")
	}
	if msg.Thinking != "" {
		return os.WriteFile(filename+".think", []byte(msg.Thinking), 0o644)
	}
	return nil
}

func generateContextName(model string, firstQuestion string) string {
	cleanModel := strings.ReplaceAll(model, " ", "_")
	safeContent := strings.ReplaceAll(firstQuestion, " ", "_")
	if len(safeContent) > 30 {
		safeContent = safeContent[:30]
	}
	sanitized := make([]rune, 0, len(safeContent))
	for _, r := range safeContent {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '-' {
			sanitized = append(sanitized, r)
		} else {
			sanitized = append(sanitized, '_')
		}
	}
	timestamp := time.Now().Format("20060102-150405")
	return fmt.Sprintf("%s_%s_%s.json", timestamp, string(sanitized), cleanModel)
}

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
		parts := strings.Split(f.Name(), "_")
		if len(parts) < 2 {
			continue
		}
		if t, err := time.Parse("20060102-150405", parts[0]); err == nil {
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
	const maxContentLen = 100
	const maxThinkLen = 50

	for i, msg := range history {
		role := "User"
		if msg.Role == "assistant" {
			role = "AI"
		} else if msg.Role == "tool" {
			role = "Tool"
		}

		var displayContent string
		switch v := msg.Content.(type) {
		case string:
			displayContent = v
		case []ContentPart:
			displayContent = "[Multimodal Content]"
		case map[string]interface{}:
			displayContent = "[Structured Content]"
		default:
			displayContent = "[Non-text Content]"
		}

		if len(msg.ToolCalls) > 0 {
			names := make([]string, 0, len(msg.ToolCalls))
			for _, tc := range msg.ToolCalls {
				names = append(names, tc.Function.Name)
			}
			displayContent = fmt.Sprintf("[Tool calls: %s]", strings.Join(names, ", "))
		}

		if utf8.RuneCountInString(displayContent) > maxContentLen {
			runes := []rune(displayContent)
			displayContent = string(runes[:maxContentLen-3]) + "..."
		}

		if msg.Thinking != "" {
			thinkPreview := strings.ReplaceAll(msg.Thinking, "\n", " ")
			if utf8.RuneCountInString(thinkPreview) > maxThinkLen {
				runes := []rune(thinkPreview)
				thinkPreview = string(runes[:maxThinkLen-3]) + "..."
			}
			fmt.Printf(" %d [%s]: 🤔 %s | 💬 %s\n", i+1, role, thinkPreview, displayContent)
		} else {
			fmt.Printf(" %d [%s]: 💬 %s\n", i+1, role, displayContent)
		}
	}
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

	fmt.Println("AI Chat CLI - REPL Mode")
	fmt.Println("Commands: /new, /add <file>, /r <cmd>, /exit, /history, /m <model>, /url <url> /list, /del <context>, /use <context>, /timeout <dur>, /mcp <spec>")
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

// ---------------------------------------------------------------------------
// Non-interactive mode
// ---------------------------------------------------------------------------

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
				message := args[i+1]
				i += 2
				history = []Message{{Role: "user", Content: message}}
				continue
			}
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

		fmt.Fprintf(os.Stderr, "Warning: Unknown command or unexpected argument: %s\n", arg)
		i++
	}

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

// ---------------------------------------------------------------------------
// Shell command helper
// ---------------------------------------------------------------------------

func runSystemCommand(cmdStr string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

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

// ---------------------------------------------------------------------------
// askAI — streams the response; if MCP is active, injects tools and handles
// tool_calls returned by the model in a loop until the model gives a final answer.
// ---------------------------------------------------------------------------

func askAI(ctx context.Context, config Config, msgs []Message) (string, string, error) {
	// Build a local copy of history for the tool loop — we extend it as we call tools
	workingMsgs := make([]Message, len(msgs))
	copy(workingMsgs, msgs)

	for {
		content, thinking, toolCalls, err := streamOnce(ctx, config, workingMsgs)
		if err != nil {
			return content, thinking, err
		}

		// No tool calls — we're done
		if len(toolCalls) == 0 {
			return content, thinking, nil
		}

		// Append the assistant's tool-call turn
		workingMsgs = append(workingMsgs, Message{
			Role:      "assistant",
			Content:   content,
			ToolCalls: toolCalls,
		})

		// Execute each tool call via MCP
		for _, tc := range toolCalls {
			fmt.Printf("\n🔧 Calling tool: %s\n", tc.Function.Name)
			fmt.Printf("   args: %s\n", tc.Function.Arguments) // always show args

			var args map[string]interface{}
			if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
				fmt.Printf("   ❌ Failed to parse tool arguments as JSON: %v\n", err)
				fmt.Printf("   Raw arguments string: %q\n", tc.Function.Arguments)
				args = map[string]interface{}{}
			}

			var toolResult string
			if activeMCP != nil {
				toolResult, err = activeMCP.CallTool(tc.Function.Name, args)
				if err != nil {
					toolResult = fmt.Sprintf("error: %v", err)
					fmt.Printf("   ❌ Tool error: %v\n", err)
					fmt.Printf("   💡 Tip: run /mcp schema to check expected argument names\n")
				} else {
					if config.Debug {
						fmt.Printf("   ✅ result: %s\n", toolResult)
					}
				}
			} else {
				toolResult = "error: no MCP client connected"
			}

			workingMsgs = append(workingMsgs, Message{
				Role:       "tool",
				ToolCallID: tc.ID,
				Name:       tc.Function.Name,
				Content:    toolResult,
			})
		}

		// Loop: ask the model again with the tool results
		fmt.Print("\n> 📝 Response:\n")
	}
}

// streamOnce does one SSE request and returns (content, thinking, toolCalls, error).
func streamOnce(ctx context.Context, config Config, msgs []Message) (string, string, []ToolCall, error) {
	url := config.BaseURL
	if !strings.Contains(url, "/chat/completions") {
		url = strings.TrimSuffix(url, "/") + "/chat/completions"
	}

	reqBody := map[string]interface{}{
		"model":    config.Model,
		"messages": msgs,
		"stream":   true,
	}

	// Inject MCP tools if connected
	if activeMCP != nil && len(activeMCP.Tools()) > 0 {
		reqBody["tools"] = ToOpenAITools(activeMCP.Tools())
		reqBody["tool_choice"] = "auto"
	}

	jsonValue, _ := json.Marshal(reqBody)
	client := &http.Client{Timeout: config.Timeout}

	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewBuffer(jsonValue))
	if err != nil {
		return "", "", nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if config.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+config.APIKey)
	}

	resp, err := client.Do(req)
	if err != nil {
		return "", "", nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		return "", "", nil, fmt.Errorf("API Error: %s", string(body))
	}

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 4*1024*1024), 4*1024*1024)

	var fullContent strings.Builder
	var thinkingContent strings.Builder
	var thinkingStarted = false
	var headerPrinted = false

	// Accumulate tool calls across streaming chunks (indexed by tool call index)
	// OpenAI streams tool calls delta by delta
	toolCallAccum := map[int]*ToolCall{}

	go func() {
		<-ctx.Done()
		resp.Body.Close()
	}()

	for scanner.Scan() {
		if ctx.Err() != nil {
			break
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

		if len(streamResp.Choices) == 0 {
			continue
		}

		delta := streamResp.Choices[0].Delta

		// Thinking content
		if delta.ReasoningContent != "" {
			if !thinkingStarted {
				fmt.Print("\n> 🤔 Thinking...\n")
				thinkingStarted = true
			}
			fmt.Print(delta.ReasoningContent)
			os.Stdout.Sync()
			thinkingContent.WriteString(delta.ReasoningContent)
		}

		// Regular text content
		if delta.Content != "" {
			if !headerPrinted {
				fmt.Print("\n> 📝 Response:\n")
				headerPrinted = true
			}
			fmt.Print(delta.Content)
			os.Stdout.Sync()
			fullContent.WriteString(delta.Content)
		}

		// Tool call deltas: keyed by the `index` field OpenAI streams per chunk.
		// This is the only reliable merge key -- do NOT use ID or name matching.
		for _, tcDelta := range delta.ToolCalls {
			key := tcDelta.Index
			if _, exists := toolCallAccum[key]; !exists {
				toolCallAccum[key] = &ToolCall{Index: key}
			}
			tc := toolCallAccum[key]
			if tcDelta.ID != "" {
				tc.ID = tcDelta.ID
			}
			if tcDelta.Type != "" {
				tc.Type = tcDelta.Type
			}
			if tcDelta.Function.Name != "" {
				tc.Function.Name += tcDelta.Function.Name
			}
			prevArgs := tc.Function.Arguments
			tc.Function.Arguments += tcDelta.Function.Arguments
			// Print indicator on first argument chunk (name arrives before args)
			if tc.Function.Name != "" && prevArgs == "" && tcDelta.Function.Arguments != "" {
				fmt.Printf("\n> 🔧 Planning tool call: %s\n", tc.Function.Name)
			}
		}
	}

	if err := scanner.Err(); err != nil && ctx.Err() == nil {
		return fullContent.String(), thinkingContent.String(), nil, fmt.Errorf("stream error: %v", err)
	}

	// Collect accumulated tool calls
	var toolCalls []ToolCall
	for i := 0; i < len(toolCallAccum); i++ {
		tc := toolCallAccum[i]
		if tc != nil && tc.Function.Name != "" {
			toolCalls = append(toolCalls, *tc)
		}
	}

	return fullContent.String(), thinkingContent.String(), toolCalls, nil
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
// Config management
// ---------------------------------------------------------------------------

func saveConfig() {
	if config == nil {
		return
	}
	dotEnvPath := filepath.Join(homeDir, ".aigdotenv")
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
		return
	}

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
	needsURL := config.BaseURL == ""
	needsModel := config.Model == ""
	needsAPIKey := config.APIKey == ""

	if !needsURL && !needsModel && !needsAPIKey {
		return
	}

	fmt.Println()
	fmt.Println("🤖 AI Chat CLI needs configuration")
	fmt.Println("We'll help you set up your OpenAI API credentials.")
	fmt.Println()

	if needsURL {
		fmt.Printf("API URL [%s]: ", config.BaseURL)
		if input, _ := reader.ReadString('\n'); strings.TrimSpace(input) != "" {
			config.BaseURL = strings.TrimSpace(input)
			config.PromptedURL = true
		}
	}

	if needsModel {
		fmt.Printf("Model [%s]: ", config.Model)
		if input, _ := reader.ReadString('\n'); strings.TrimSpace(input) != "" {
			config.Model = strings.TrimSpace(input)
			config.PromptedModel = true
		}
	}

	if needsAPIKey {
		fmt.Printf("API Key (will not be displayed): ")
		apiKey, _ := reader.ReadString('\n')
		apiKey = strings.TrimSpace(string(apiKey))
		if apiKey != "" {
			config.APIKey = apiKey
			config.PromptedAPIKey = true
		}
	}

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

	if len(os.Args) == 1 {
		promptForMissingConfig(&c)
	}

	return &c
}

// ---------------------------------------------------------------------------
// Command handler
// ---------------------------------------------------------------------------

func handleCommand(text string, config *Config, history *[]Message) {
	parts := strings.SplitN(text, " ", 2)
	cmd := parts[0]
	arg := ""
	if len(parts) > 1 {
		arg = parts[1]
	}

	switch cmd {

	// -----------------------------------------------------------------------
	// MCP commands
	// -----------------------------------------------------------------------

	case "/mcp":
		if arg == "" {
			// Show current MCP status
			if activeMCP == nil {
				fmt.Println("ℹ️  No MCP server connected.")
				fmt.Println("Usage:")
				fmt.Println("  /mcp tcp://<host>:<port>   — connect to a running MCP TCP server")
				fmt.Println("  /mcp <cmd> [args...]        — launch and connect to an MCP stdio server")
				fmt.Println("  /mcp off                    — disconnect current MCP server")
				fmt.Println("  /mcp tools                  — list available tools")
			} else {
				fmt.Printf("✅ MCP connected: %s\n", activeMCP.spec)
				fmt.Printf("   %d tool(s) available\n", len(activeMCP.Tools()))
			}
			return
		}

		switch arg {
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

		// Disconnect any existing MCP first
		if activeMCP != nil {
			fmt.Println("🔌 Disconnecting previous MCP server...")
			activeMCP.Close()
			activeMCP = nil
		}

		// Connect
		var newMCP *MCPClient
		var err error

		if strings.HasPrefix(arg, "tcp://") {
			fmt.Printf("🔌 Connecting to MCP TCP server: %s\n", arg)
			newMCP, err = ConnectTCP(arg)
		} else {
			fmt.Printf("🚀 Launching MCP stdio server: %s\n", arg)
			newMCP, err = ConnectStdio(arg)
		}

		if err != nil {
			fmt.Printf("❌ MCP connect failed: %v\n", err)
			return
		}

		activeMCP = newMCP
		tools := activeMCP.Tools()
		fmt.Printf("✅ MCP connected: %s\n", activeMCP.spec)
		fmt.Printf("   %d tool(s) available:\n", len(tools))
		for _, t := range tools {
			fmt.Printf("   • %s — %s\n", t.Name, t.Description)
		}

	// -----------------------------------------------------------------------
	// Original commands (unchanged)
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
			newContextName := generateContextName(config.Model, firstUserMsg)
			newContextPath := filepath.Join(homeDir, ".aig", newContextName)
			if err := saveHistory(); err != nil {
				fmt.Printf("⚠️  Could not save current session: %v\n", err)
			} else {
				fmt.Printf("✅ Saved current session to: %s\n", filepath.Base(newContextPath))
			}
		}
		*history = []Message{}
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
		fmt.Println("  /s <hist_index:filename>      - Save the history index to a file")
		fmt.Println("  /cd <dirname>                 - Change to directory")
		fmt.Println("  /mcp <spec>                   - Connect MCP server (tcp://host:port or cmd)")
		fmt.Println("  /mcp off                      - Disconnect MCP server")
		fmt.Println("  /mcp tools                    - List available MCP tools")
		fmt.Println("  /mcp refresh                  - Refresh MCP tool list")

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
		fmt.Printf("Added '%s' to conversation context.\n", arg)

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

	case "/p":
		fmt.Printf("Current config: %s\n", u.JsonDump(config, ""))
		if activeMCP != nil {
			fmt.Printf("MCP: %s (%d tools)\n", activeMCP.spec, len(activeMCP.Tools()))
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

func runREPLWithReader(config *Config, history *[]Message, rl *readline.Instance, histFile string) {
	printPrompt := func() { fmt.Print("You: ") }
	printOutput := func(s string) { fmt.Printf("AI: %s\n", s) }

	doTurn := func(ctx context.Context, text string) {
		// Ensure context path is set
		if currentContextPath == "" {
			newContextName := generateContextName(config.Model, text)
			currentContextPath = filepath.Join(homeDir, ".aig", newContextName)
		}

		*history = append(*history, Message{Role: "user", Content: text})

		ans, thinking, err := askAI(ctx, *config, *history)
		if err != nil {
			fmt.Printf("Error: %v\n", err)
			*history = (*history)[:len(*history)-1]
			return
		}

		if ctx.Err() != nil {
			return
		}

		printOutput(ans)
		*history = append(*history, Message{
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
