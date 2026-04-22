package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/joho/godotenv"
)

// --- Configuration & Data Structures ---

type Config struct {
	BaseURL string
	Model   string
	APIKey  string
	Timeout time.Duration
}

type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type Response struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	Choices []struct {
		Message struct {
			Content string `json:"content"`
			Role    string `json:"role"`
		} `json:"message"`
	} `json:"choices"`
}

type History struct {
	History []Message `json:"history"`
}

// Global State
var (
	history       []Message
	historyLoaded bool = false
	homeDir, _         = os.UserHomeDir()
)

// --- Main Logic ---

func main() {
	// Load config from multiple sources: Current Dir .env, then Home .aigdotenv

	_ = godotenv.Load()                                  // Current directory
	homeEnv, _ := godotenv.Read(homeDir + "/.aigdotenv") // User home dir

	// Merge or prefer home dir env vars over current dir
	for k, v := range homeEnv {
		// Only set if not already defined
		if os.Getenv(k) == "" {
			_ = os.Setenv(k, v)
		}
	}

	config := loadConfig()

	// Load history if exists
	historyFilePath := getHistoryFilePath()
	if _, err := os.Stat(historyFilePath); err == nil {
		if err := loadHistory(historyFilePath); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: Could not load history file: %v\n", err)
		} else {
			historyLoaded = true
		}
	}

	// Check for command-line arguments
	if len(os.Args) > 1 {
		handleArguments(config)
		return
	}

	// --- REPL Mode ---
	fmt.Println("AI Chat CLI - REPL Mode")
	fmt.Println("Commands: /new, /add <file>, /exit")
	fmt.Println("----------------------------------------")
	if historyLoaded {
		fmt.Printf("Loaded %d messages from previous session.\n", len(history)/2)
		fmt.Println("Type /new to start fresh if desired.")
		fmt.Println()
	}

	scanner := bufio.NewScanner(os.Stdin)

	for {
		fmt.Print("You: ")
		if !scanner.Scan() {
			fmt.Println()
			break
		}

		text := strings.TrimSpace(scanner.Text())
		if text == "" {
			continue
		}

		if strings.HasPrefix(text, "/") {
			if text == "/exit" {
				break
			}
			handleCommand(text, config, &history)
			continue
		}

		history = append(history, Message{Role: "user", Content: text})
		ans, err := askAI(config, history)
		if err != nil {
			fmt.Printf("Error: %v\n", err)
			history = history[:len(history)-1]
			continue
		}

		fmt.Printf("AI: %s\n", ans)
		history = append(history, Message{Role: "assistant", Content: ans})
		saveHistory(historyFilePath)
	}

	// Save final state
	saveHistory(historyFilePath)
}

// --- Argument Parsing & Execution ---

func handleArguments(config Config) {
	args := os.Args[1:]
	i := 0
	for i < len(args) {
		arg := args[i]

		if arg == "-cmd" || arg == "-c" {
			if i+1 >= len(args) {
				fmt.Println("Error: -cmd requires an argument (new, add, repl)")
				os.Exit(1)
			}
			i++
			cmd := args[i]

			switch cmd {
			case "new":
				history = []Message{}
				homeDir, _ := os.UserHomeDir()
				os.RemoveAll(homeDir + "/.aihistory")
				fmt.Println("New conversation started (cleared history).")
			case "add":
				if i+1 >= len(args) {
					fmt.Println("Error: /add requires a filename (use -f <filename>)")
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
				fmt.Printf("Added '%s' to conversation context.\n", filePath)
			case "repl":
				fmt.Println("Starting REPL mode with current context...")
				runREPL(config)
			default:
				fmt.Printf("Unknown command: %s\n", cmd)
			}
			i++
			continue
		}

		if arg == "-f" {
			// Skip this flag and the filename, it's handled by -cmd add logic above
			if i+1 >= len(args) {
				fmt.Println("Error: -f requires a filename")
				os.Exit(1)
			}
			i += 2
			continue
		}

		if !strings.HasPrefix(arg, "-") {
			// Single shot execution
			question := strings.Join(args[i:], " ")
			ans, err := askAI(config, []Message{{Role: "user", Content: question}})
			if err != nil {
				fmt.Fprintf(os.Stderr, "Error: %v\n", err)
				os.Exit(1)
			}
			fmt.Print(ans)
			return
		}

		i++
	}

	// Default to REPL if no specific actions were taken
	runREPL(config)
}

// Convenience function to run REPL
func runREPL(config Config) {
	fmt.Println("AI Chat REPL (Interactive Mode)")
	fmt.Println("Commands: /new, /add <file>, /exit")
	fmt.Println("----------------------------------------")

	scanner := bufio.NewScanner(os.Stdin)
	for {
		fmt.Print("You: ")
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
				break
			}
			handleCommand(text, config, &history)
			continue
		}

		history = append(history, Message{Role: "user", Content: text})
		ans, err := askAI(config, history)
		if err != nil {
			fmt.Printf("Error: %v\n", err)
			history = history[:len(history)-1]
			continue
		}

		fmt.Printf("AI: %s\n", ans)
		history = append(history, Message{Role: "assistant", Content: ans})
		saveHistory(getHistoryFilePath())
	}
}

// --- Helper Functions ---

func handleCommand(text string, config Config, history *[]Message) {
	parts := strings.Split(text, " ")
	cmd := parts[0]
	args := parts[1:]

	switch cmd {
	case "/new":
		*history = []Message{}
		fmt.Println("New conversation started.")
		os.Remove(getHistoryFilePath())
	case "/help":
		fmt.Println("Commands: /new, /add <file>, /exit")
	case "/add":
		if len(args) == 0 {
			fmt.Println("Usage: /add <filename>")
			return
		}
		filePath := args[0]
		content, err := os.ReadFile(filePath)
		if err != nil {
			fmt.Printf("Error reading file %s: %v\n", filePath, err)
			return
		}
		*history = append([]Message{
			{Role: "system", Content: fmt.Sprintf("Reference File Content:\n```\n%s\n```\n", string(content))},
		}, (*history)...)
		fmt.Printf("Added '%s' to conversation context.\n", filePath)
		saveHistory(getHistoryFilePath())
	default:
		fmt.Printf("Unknown command: %s\n", cmd)
	}
}

func askAI(config Config, history []Message) (string, error) {
	url := config.BaseURL
	if !strings.Contains(url, "/chat/completions") {
		url = strings.TrimSuffix(url, "/") + "/chat/completions"
	}

	jsonData := map[string]interface{}{
		"model":    config.Model,
		"messages": history,
		"stream":   false,
	}

	jsonValue, _ := json.Marshal(jsonData)

	// Create client with configurable timeout
	client := &http.Client{Timeout: config.Timeout}

	req, err := http.NewRequest("POST", url, bytes.NewBuffer(jsonValue))
	if err != nil {
		return "", err
	}

	req.Header.Set("Content-Type", "application/json")
	if config.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+config.APIKey)
	}

	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}

	if resp.StatusCode != 200 {
		return fmt.Sprintf("API Error: %s", string(body)), nil
	}

	var apiResponse Response
	if err := json.Unmarshal(body, &apiResponse); err != nil {
		return "", err
	}

	if len(apiResponse.Choices) > 0 {
		return apiResponse.Choices[0].Message.Content, nil
	}

	return "", fmt.Errorf("no choices returned")
}

// --- File Management ---

func getHistoryFilePath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ".aihistory.json"
	}
	return filepath.Join(home, ".aihistory.json")
}

func loadHistory(filePath string) error {
	data, err := os.ReadFile(filePath)
	if err != nil {
		return err
	}
	var h History
	if err := json.Unmarshal(data, &h); err != nil {
		return err
	}
	history = h.History
	return nil
}

func saveHistory(filePath string) error {
	dir := filepath.Dir(filePath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}

	h := History{History: history}
	data, err := json.MarshalIndent(h, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filePath, data, 0644)
}

func loadConfig() Config {
	c := Config{
		BaseURL: os.Getenv("OPENAI_URL"),
		Model:   os.Getenv("OPENAI_MODEL"),
		APIKey:  os.Getenv("OPENAI_API_KEY"),
		Timeout: 15 * time.Minute, // Default to 15 minutes
	}

	// Override timeout with env variable
	if timeoutStr := os.Getenv("TIMEOUT"); timeoutStr != "" {
		if seconds, err := strconv.Atoi(timeoutStr); err == nil {
			c.Timeout = time.Duration(seconds) * time.Second
		}
	}

	if c.BaseURL == "" {
		c.BaseURL = "https://api.openai.com/v1/chat/completions"
	}
	if c.Model == "" {
		c.Model = "gpt-3.5-turbo"
	}

	// Auto-detect Ollama
	if strings.Contains(c.BaseURL, "localhost:11434") || strings.Contains(c.BaseURL, "localhost:4333") {
		if c.Model == "" {
			c.Model = "llama3"
		}
	}

	return c
}
