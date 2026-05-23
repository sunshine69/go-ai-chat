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

// askAI — streams the response; if MCP is active, injects tools and handles
// tool_calls returned by the model in a loop until the model gives a final answer.
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
				fmt.Println("\n✅ Calling tool: done")
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

// streamOnce does one SSE request, printing text, prepare tools for one turn and returns
// (content, thinking, toolCalls, error). Tool call if detected and parsed ok will end the turn
func streamOnce(ctx context.Context, config Config, msgs []Message) (string, string, []ToolCall, error) {
	globalStats.StreamStarted()
	defer globalStats.StreamFinished()

	reqBody := map[string]interface{}{
		"model":    config.Model,
		"messages": msgs,
		"stream":   true,
	}

	// Inject MCP tools if connected
	if activeMCP != nil && len(activeMCP.Tools()) > 0 {
		// Pass the tools slice and your comma-separated blocklist string directly
		// Example value for config.BlockedTools: "dangerous_delete, format_hard_drive"
		visibleTools := parseAndFilterToolsRegex(activeMCP.Tools(), config.BlockedTools)
		if len(visibleTools) > 0 {
			reqBody["tools"] = ToOpenAITools(visibleTools)
			reqBody["tool_choice"] = "auto"
		}
	}

	jsonValue, _ := json.Marshal(reqBody)
	client := &http.Client{Timeout: config.Timeout}

	req, err := http.NewRequestWithContext(ctx, "POST", config.BaseURL, bytes.NewBuffer(jsonValue))
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
	scanner.Buffer(make([]byte, 4194304), 4194304) // 4Mb large enough for one turn

	var fullContent strings.Builder
	var thinkingContent strings.Builder
	var thinkingStarted = false
	var headerPrinted = false
	var serverSignaledStop = false

	// Accumulate tool calls across streaming chunks (indexed by tool call index)
	toolCallAccum := map[int]*ToolCall{}

	go func() {
		<-ctx.Done()
		resp.Body.Close()
	}()
	// 1. Create a slice to hold the last 10 lines in memory (RAM)
	debugHistory := make([]string, 10)
	historyIdx := 0
	// Dealing with llama and Qwen3 bug when it corrupted stream
	// The Qwen "Multi-Session Mixup" (The Core Root Cause)
	//The Late-Arriving XML Tags inside reasoning_content
	//The Server Sends finish_reason: "stop" instead of "tool_calls"

	var expectedSessionID string
	for scanner.Scan() {
		if ctx.Err() != nil {
			break
		}
		// If the server explicitly declared it is done with tool calls,
		// ignore any runaway conversational text that follows (fixes Qwen runaway text bug)
		if serverSignaledStop {
			continue
		}

		line := strings.TrimSpace(scanner.Text())
		debugHistory[historyIdx%10] = line
		historyIdx++
		if line == "" || line == "data: [DONE]" || !strings.HasPrefix(line, "data: ") {
			continue
		}

		streamData := strings.TrimPrefix(line, "data: ")
		var streamResp Response
		if err := json.Unmarshal([]byte(streamData), &streamResp); err != nil {
			continue
		}
		// Set the unique ID on the very first valid line
		if expectedSessionID == "" && streamResp.ID != "" {
			expectedSessionID = streamResp.ID
		}
		// CRITICAL: Drop any chunks belonging to a leaked parallel request!
		if expectedSessionID != "" && streamResp.ID != expectedSessionID {
			continue
		}
		if len(streamResp.Choices) == 0 {
			continue
		}

		choice := streamResp.Choices[0]
		delta := choice.Delta

		// Handle explicit server finish signals safely
		if choice.FinishReason != "" && (choice.FinishReason == "tool_calls" || choice.FinishReason == "stop") {
			serverSignaledStop = true
		}

		// 1. Unpack Thinking content
		if delta.ReasoningContent != "" {
			rc := delta.ReasoningContent
			// If Qwen attempts to close its tool syntax in the wrong block, handle it gracefully
			if strings.Contains(rc, "</function>") || strings.Contains(rc, "</tool_call>") {
				if len(toolCallAccum) > 0 {
					serverSignaledStop = true
				}
				continue
			}
			tokenCount := len(strings.Fields(delta.ReasoningContent))
			globalStats.TokenArrived(tokenCount)

			thinkingContent.WriteString(delta.ReasoningContent)
			if config.ShowThinking {
				if !thinkingStarted {
					fmt.Print("\n> 🤔 Thinking...\n")
					thinkingStarted = true
				}
				fmt.Print(delta.ReasoningContent)
				os.Stdout.Sync()
			} else if !thinkingStarted {
				fmt.Print("\n> 🤔 Thinking hidden, run /showthink on to enable\n")
				thinkingStarted = true
			}
			continue
		}

		// 🛡️ CRITICAL FIX 3: Trigger tool capture even if server flags a standard "stop" status
		if choice.FinishReason != "" && (choice.FinishReason == "tool_calls" || choice.FinishReason == "stop") {
			if len(toolCallAccum) > 0 {
				serverSignaledStop = true
				continue
			}
		}

		// 2. Unpack Regular text content
		if delta.Content != "" {
			tokenCount := len(strings.Fields(delta.Content))
			globalStats.TokenArrived(tokenCount)

			if !headerPrinted {
				fmt.Print("\n> 📝 Response:\n")
				headerPrinted = true
			}
			fullContent.WriteString(delta.Content)
			fmt.Print(delta.Content)
			os.Stdout.Sync()
			continue
		}

		// 3. Unpack Tool call deltas safely
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

			// Print visual anchor on first argument chunk arrival
			if tc.Function.Name != "" && prevArgs == "" && tcDelta.Function.Arguments != "" {
				fmt.Printf("\n> 🔧 Planning tool call: %s\n", tc.Function.Name)
			}
		}
	} // scanner end

	if err := scanner.Err(); err != nil && ctx.Err() == nil {
		return fullContent.String(), thinkingContent.String(), nil, fmt.Errorf("stream error: %v", err)
	}

	// Collect accumulated tool calls safely using a range loop over the map
	var toolCalls []ToolCall
	for _, tc := range toolCallAccum {
		if tc == nil || tc.Type != "function" || strings.TrimSpace(tc.Function.Name) == "" {
			continue
		}

		args := strings.TrimSpace(tc.Function.Arguments)
		if args == "" {
			fmt.Printf("\n> ⚠️ Ignoring incomplete tool call (missing args): %s\n", tc.Function.Name)
			continue
		}

		// Perform JSON validation only ONCE here after streaming is entirely complete
		var tmp interface{}
		if err := json.Unmarshal([]byte(args), &tmp); err != nil {
			fmt.Printf("\n> ⚠️ Ignoring malformed tool call %s: %v\n", tc.Function.Name, err)
			continue
		}

		if config.Debug {
			fmt.Printf("CALL TOOL: %+v\n", tc)
		}
		toolCalls = append(toolCalls, *tc)
	}

	// 3. If the loop finished but we didn't get tools or text (a freeze/cutoff happened)
	// You can print out the exact history from RAM to see what went wrong.
	if len(toolCalls) == 0 && fullContent.Len() == 0 {
		fmt.Fprintln(os.Stderr, "\n--- 🚨 STREAM CUTOFF DETECTED (LAST 10 LINES) ---")
		for i := 0; i < 10; i++ {
			// Print the lines in chronological order
			line := debugHistory[(historyIdx+i)%10]
			if line != "" {
				if config.Debug {
					debugFile.WriteString(line + "\n")
					debugFile.Sync()
				} else {
					fmt.Fprintln(os.Stderr, line)
				}
			}
		}
	}
	return fullContent.String(), thinkingContent.String(), toolCalls, nil
}
