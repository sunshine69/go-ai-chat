package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
)

// ---------------------------------------------------------------------------
// askAI — streams the response; if MCP is active, injects tools and handles
// tool_calls returned by the model in a loop until the model gives a final answer.
// ---------------------------------------------------------------------------
func askAI(ctx context.Context, config Config, msgs []Message) (string, string, []Message, error) {
	// Build a local copy of history for the tool loop — we extend it as we call tools
	workingMsgs := make([]Message, len(msgs))
	copy(workingMsgs, msgs)
	for {
		if config.ContextLimit > 0 && estimateTokens(workingMsgs) > config.ContextLimit {
			fmt.Printf("📊 Context size: ~%d tokens (limit: %d)\n", estimateTokens(workingMsgs), config.ContextLimit)
			workingMsgs = trimContext(ctx, config, workingMsgs)
		}
		content, thinking, toolCalls, err := streamOnce(ctx, config, workingMsgs)
		if err != nil {
			return content, thinking, workingMsgs, err
		}
		// No tool calls — we're done
		if len(toolCalls) == 0 {
			return content, thinking, workingMsgs, nil
		}
		// Append the assistant's tool-call turn
		workingMsgs = append(workingMsgs, Message{
			Role:      "assistant",
			Content:   content,
			ToolCalls: toolCalls,
		})

		// Execute each tool call via MCP
		for _, tc := range toolCalls {
			// --- 1. PREPARE ARGUMENT DISPLAY (Pretty Print) ---
			var argsDisplay string
			var argsMap map[string]interface{}

			// Attempt to parse and pretty-print the JSON arguments
			if err := json.Unmarshal([]byte(tc.Function.Arguments), &argsMap); err != nil {
				// If AI sent invalid JSON, just show the raw string so the user can still see it
				argsDisplay = tc.Function.Arguments
			} else {
				pretty, _ := json.MarshalIndent(argsMap, "    ", "  ")
				argsDisplay = string(pretty)
			}

			// --- 2. PERMISSION CHECK ---
			perm := getEffectivePermission(tc.Function.Name, &config)
			allowed := true

			switch perm {
			case "deny":
				fmt.Printf("\n🚫 Permission Denied: Tool '%s' is blocked.\n", tc.Function.Name)
				allowed = false
			case "ask":
				fmt.Printf("\n⚠️  Tool '%s' requires permission.\n", tc.Function.Name)
				fmt.Printf("   Arguments:\n%s\n", argsDisplay) // Print arguments BEFORE asking
				fmt.Printf("   Allow? [y/N]: ")

				var response string
				// Use Scanln to wait for user input
				fmt.Scanln(&response)
				if strings.ToLower(response) != "y" {
					fmt.Printf("❌ User denied permission for '%s'.\n", tc.Function.Name)
					allowed = false
				}
			}

			// --- 3. EXECUTION ---
			var toolResult string
			if !allowed {
				toolResult = fmt.Sprintf("error: permission denied for tool %s (policy: %s)", tc.Function.Name, perm)
			} else {
				fmt.Printf("\n🔧 Calling tool: %s\n", tc.Function.Name)

				// We already parsed argsMap above, let's reuse it or re-parse for the actual call
				var finalArgs map[string]interface{}
				if err := json.Unmarshal([]byte(tc.Function.Arguments), &finalArgs); err != nil {
					finalArgs = map[string]interface{}{}
				}

				if activeMCP != nil {
					toolResult, err = activeMCP.CallTool(tc.Function.Name, finalArgs)
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
			}
			// Always append the tool result (including permission-denied errors) so the
			// model receives a result for every tool call it made.
			workingMsgs = append(workingMsgs, Message{
				Role:       "tool",
				ToolCallID: tc.ID,
				Name:       tc.Function.Name,
				Content:    toolResult,
			})
		}
		// Tool results appended — loop back to call the model for its final response.
	}
}

// streamOnce does one SSE request and returns (content, thinking, toolCalls, error).
func streamOnce(ctx context.Context, config Config, msgs []Message) (string, string, []ToolCall, error) {
	globalStats.StreamStarted() // ← top of function
	defer globalStats.StreamFinished()

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
			tokenCount := len(strings.Fields(delta.ReasoningContent))
			globalStats.TokenArrived(tokenCount)

			// ALWAYS capture the content so the history is complete
			thinkingContent.WriteString(delta.ReasoningContent)
			// ONLY print if the user has enabled it
			if config.ShowThinking {
				if !thinkingStarted {
					fmt.Print("\n> 🤔 Thinking...\n")
					thinkingStarted = true
				}
				fmt.Print(delta.ReasoningContent)
				os.Stdout.Sync()
			} else {
				if !thinkingStarted {
					fmt.Print("\n> 🤔 Thinking hidden, run /showthink on to enable\n")
					thinkingStarted = true
				}
			}
		}

		// Regular text content
		if delta.Content != "" {
			tokenCount := len(strings.Fields(delta.Content))
			globalStats.TokenArrived(tokenCount)

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
