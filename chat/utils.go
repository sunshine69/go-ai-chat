package main

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"mime"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	u "github.com/sunshine69/golang-tools/utils"
)

// Input like 3-5 or -1--5 return slice index - if negative return from the right end
func ParseRangeFromInputString(s string) (int, int, bool) {
	// Look for a hyphen that isn't the first character (to allow negative numbers)
	for i := 1; i < len(s); i++ {
		if s[i] == '-' {
			leftStr := s[:i]
			rightStr := s[i+1:]
			l, errL := strconv.Atoi(leftStr)
			r, errR := strconv.Atoi(rightStr)
			if errL == nil && errR == nil {
				return l, r, true
			}
		}
	}
	// If no range separator found, try parsing as a single integer
	val, err := strconv.Atoi(s)
	if err == nil {
		return val, val, true
	}
	return 0, 0, false
}

// ---- file_processors.go (replace the relevant functions) ----

// TextProcessor now returns []ContentPart instead of a raw string.
func (p *TextProcessor) Process(path string) ([]ContentPart, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	text := fmt.Sprintf("Reference File Content (%s):\n```\n%s\n```\n", filepath.Base(path), string(content))
	return []ContentPart{
		{Type: "text", Text: text},
	}, nil
}

// ImageProcessor already returned []ContentPart — signature now matches.
func (p *ImageProcessor) Process(path string) ([]ContentPart, error) {
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

// FileProcessor interface — update the signature here too.
type FileProcessor interface {
	Process(path string) ([]ContentPart, error)
}

// processFile always returns []ContentPart now.
func processFile(path string) ([]ContentPart, error) {
	return getFileProcessor(path).Process(path)
}

// Marker

func getFileProcessor(path string) FileProcessor {
	ext := filepath.Ext(path)
	switch ext {
	case ".png", ".jpg", ".jpeg", ".gif", ".webp":
		return &ImageProcessor{}
	default:
		return &TextProcessor{}
	}
}

var (
	history            []Message
	historyLoaded      bool = false
	homeDir, _              = os.UserHomeDir()
	config             *Config
	currentContextPath string

	// Global MCP client (single active connection)
	activeMCP *ResilientMCPClient
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

func sanitizeString(s string) string {
	sanitized := make([]rune, 0, len(s))
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '-' {
			sanitized = append(sanitized, r)
		} else {
			sanitized = append(sanitized, '_')
		}
	}
	return string(sanitized)
}

// 2. Refactored to use the helper and take a 'base' string
func generateContextName(model string, base string) string {
	cleanModel := strings.ReplaceAll(model, " ", "_")
	safeBase := sanitizeString(base)
	if len(safeBase) > 30 {
		safeBase = safeBase[:30]
	}
	timestamp := time.Now().Format("20060102-150405")
	return fmt.Sprintf("%s_%s_%s.json", timestamp, safeBase, cleanModel)
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
		case map[string]any:
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
	if config.SummaryModelTimeout != "" {
		envVars["SUMMARY_MODEL_TIMEOUT"] = config.SummaryModelTimeout
		changed = true
	}
	if config.ShowThinking {
		envVars["SHOW_THINKING"] = "true"
	} else {
		envVars["SHOW_THINKING"] = "false"
	}
	if config.ContextLimit > 0 {
		envVars["CONTEXT_LIMIT"] = strconv.Itoa(config.ContextLimit)
		changed = true
	}
	if config.SummaryModel != "" {
		envVars["SUMMARY_MODEL"] = config.SummaryModel
		changed = true
	}
	if !changed {
		return
	}
	for name, perm := range config.MCPPermissions {
		if perm != "" {
			envVars["MCP_PERM_"+name] = perm
		}
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

// isWriteTool checks if a tool name implies a modification/destructive action.
func isWriteTool(name string) bool {
	name = strings.ToLower(name)
	writeKeywords := []string{
		"write", "create", "delete", "remove", "update", "edit",
		"post", "put", "patch", "exec", "run", "shell", "mkdir", "rm",
	}
	for _, kw := range writeKeywords {
		if strings.Contains(name, kw) {
			return true
		}
	}
	return false
}

// getEffectivePermission returns the user-defined permission or the default.
func getEffectivePermission(name string, config *Config) string {
	// 1. Check if explicitly set
	if perm, ok := config.MCPPermissions[name]; ok {
		return perm
	}
	// 2. Fallback to defaults
	if isWriteTool(name) {
		return "ask"
	}
	return "auto"
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
		fmt.Printf("API Key (will not be displayed). If no need type anything and hit enter: ")
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
		BaseURL:             os.Getenv("OPENAI_URL"),
		Model:               os.Getenv("OPENAI_MODEL"),
		SummaryModel:        os.Getenv("SUMMARY_MODEL"),
		SummaryModelTimeout: u.Getenv("SUMMARY_MODEL_TIMEOUT", "60s"),
		APIKey:              os.Getenv("OPENAI_API_KEY"),
		Timeout:             45 * time.Minute,
		PromptedURL:         false,
		PromptedModel:       false,
		PromptedAPIKey:      false,
		MCPPermissions:      make(map[string]string),
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
	// NEW: Parse MCP Permissions from environment
	// Since we use godotenv, all vars in .aigdotenv are in the env
	// We'll iterate through keys if possible, but since we can't easily
	// list all env vars in Go without platform specific calls,
	// we'll rely on the fact that we are reading the file manually below
	// or we can just scan the file in loadConfig.

	// Let's update loadConfig to read the file manually for permissions
	dotEnvPath := filepath.Join(homeDir, ".aigdotenv")
	if data, err := os.ReadFile(dotEnvPath); err == nil {
		lines := strings.Split(string(data), "\n")
		for _, line := range lines {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			parts := strings.SplitN(line, "=", 2)
			if len(parts) == 2 {
				key, val := parts[0], parts[1]
				if strings.HasPrefix(key, "MCP_PERM_") {
					toolName := strings.TrimPrefix(key, "MCP_PERM_")
					c.MCPPermissions[toolName] = val
				}
				if key == "SHOW_THINKING" {
					c.ShowThinking = (val == "true")
				}
				if key == "CONTEXT_LIMIT" {
					if n, err := strconv.Atoi(val); err == nil {
						c.ContextLimit = n
					}
				}
			}
		}
	}

	return &c
}
