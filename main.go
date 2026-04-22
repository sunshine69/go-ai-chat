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
	"strings"
	"time"

	"github.com/joho/godotenv"
)

// --- Configuration & Data Structures ---

type Config struct {
	BaseURL string
	Model   string
	APIKey  string
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

// --- Global State ---

var (
	history       []Message
	historyLoaded bool = false
)

// --- Main Logic ---

func main() {
	_ = godotenv.Load()

	config := loadConfig()

	// Load history if exists (only if not in a "clear" command)
	historyFilePath := getHistoryFilePath()
	if _, err := os.Stat(historyFilePath); err == nil {
		if err := loadHistory(historyFilePath); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: Could not load history file: %v\n", err)
		} else {
			historyLoaded = true
		}
	}

	// Check for command-line arguments (Non-Interactive Mode)
	// If args exist, we process commands, otherwise we drop into REPL
	if len(os.Args) > 1 {
		handleArguments(config)
		return
	}

	// --- REPL Mode ---

	fmt.Println("AI Chat CLI - REPL Mode")
	fmt.Println("Commands: /new, /add <file>, /exit")
	fmt.Println("----------------------------------------")

	// If we loaded history, give a heads up
	if historyLoaded {
		fmt.Printf("Loaded %d messages from previous session.\n", len(history)/2) // approx count (user+ai)
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

		// Handle internal commands
		if strings.HasPrefix(text, "/") {
			if text == "/exit" {
				break
			}
			handleCommand(text, config, &history)
			continue
		}

		// User Question Logic
		history = append(history, Message{Role: "user", Content: text})

		ans, err := askAI(config, history)
		if err != nil {
			fmt.Printf("Error: %v\n", err)
			history = history[:len(history)-1] // Rollback
			continue
		}

		fmt.Printf("AI: %s\n", ans)
		history = append(history, Message{Role: "assistant", Content: ans})

		// Save history after every interaction
		saveHistory(historyFilePath)
	}

	// Save final state on exit
	saveHistory(historyFilePath)
}

// --- Argument Parsing & Execution ---

func handleArguments(config Config) {
	// Simple manual argument parsing to allow -cmd -f combinations
	args := os.Args[1:]
	i := 0
	for i < len(args) {
		arg := args[i]

		if arg == "-cmd" {
			if i+1 >= len(args) {
				fmt.Println("Error: -cmd requires an argument (new, add, repl)")
				os.Exit(1)
			}
			i++
			cmd := args[i]

			switch cmd {
			case "new":
				history = []Message{}
				fmt.Println("New conversation started (cleared history).")
			case "add":
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

		// Single shot execution if just text is provided
		if !strings.HasPrefix(arg, "-") {
			// Treat remaining args as the question
			question := strings.Join(args[i:], " ")
			ans, err := askAI(config, []Message{{Role: "user", Content: question}})
			if err != nil {
				fmt.Fprintf(os.Stderr, "Error: %v\n", err)
				os.Exit(1)
			}
			fmt.Print(ans)
			return // Exit immediately after single shot
		}

		i++
	}

	// If loop finished and no specific command triggered exit, check if we want repl
	// If user ran: `./aichat -cmd repl` we already handled it.
	// If user ran: `./aichat -cmd add -f file.txt` we handled it.
	// If user ran: `./aichat -cmd new` we handled it.
	// If user ran: `./aichat "question"` we handled it.
}

// Convenience function to run REPL after processing commands
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

		// Save immediately
		historyFilePath := getHistoryFilePath()
		saveHistory(historyFilePath)
	}

	// Save final state
	historyFilePath := getHistoryFilePath()
	saveHistory(historyFilePath)
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
		historyFilePath := getHistoryFilePath()
		os.Remove(historyFilePath) // Clear file
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
		historyFilePath := getHistoryFilePath()
		saveHistory(historyFilePath)
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

	client := &http.Client{Timeout: 60 * time.Second}
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

// --- History File Management ---

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
	// Ensure directory exists
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
