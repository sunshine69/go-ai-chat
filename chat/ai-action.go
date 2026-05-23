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
	var accumulatedContent strings.Builder
	var accumulatedThinking strings.Builder
	for {
		if config.ContextLimit > 0 && estimateTokens(workingMsgs) > config.ContextLimit {
			fmt.Printf("📊 Context size: ~%d tokens (limit: %d)\n", estimateTokens(workingMsgs), config.ContextLimit)
			workingMsgs = trimContext(ctx, config, workingMsgs)
		}
		content, thinking, toolCalls, err := streamOnce(ctx, config, workingMsgs)
		if err != nil {
			return accumulatedContent.String() + content, accumulatedThinking.String() + thinking, workingMsgs, err
		}
		// Always gather whatever text it managed to output
		if content != "" {
			accumulatedContent.WriteString(content)
		}
		if thinking != "" {
			accumulatedThinking.WriteString(thinking)
		}

		// --- 🛌 THE LAZY MODEL AUTO-NUDGE TRAP HAHA ---
		// If it chose to "stop" naturally, but left you with ZERO tools and ZERO text
		// after doing a bunch of thinking, it's being lazy. Wake it up!
		if ctx.Err() == nil && !strings.Contains(accumulatedContent.String(), "Task completed") && len(toolCalls) == 0 && ((content == "" && thinking != "") || aiRestResponsePatern.MatchString(accumulatedContent.String())) {
			fmt.Println("\n> ⚡ [System Nudge]: AI went to sleep after planning. Forcing execution...")

			// Save its thoughts into the history so it doesn't forget the plan
			workingMsgs = append(workingMsgs, Message{
				Role:    "assistant",
				Content: thinking,
			})

			// Add a firm nudge telling it to execute step 1 immediately
			workingMsgs = append(workingMsgs, Message{
				Role:    "user",
				Content: "Continue. If completed replied with string 'Task completed'",
			})

			// Loop right back up to force it to talk again!
			continue
		}
		// No tool calls — we're done
		if len(toolCalls) == 0 {
			return accumulatedContent.String(), accumulatedThinking.String(), workingMsgs, nil
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
	var rawTextBuffer strings.Builder
	var thinkingStarted = false
	var headerPrinted = false
	var serverSignaledStop = false

	// Accumulate tool calls across streaming chunks (indexed by tool call index)
	toolCallAccum := map[int]*ToolCall{}

	go func() {
		<-ctx.Done()
		resp.Body.Close()
	}()

	debugHistory := make([]string, 10)
	historyIdx := 0
	var expectedSessionID string

	for scanner.Scan() {
		if ctx.Err() != nil {
			break
		}
		if serverSignaledStop {
			continue
		}

		line := strings.TrimSpace(scanner.Text())
		if config.DebugLevel == "2" {
			debugFile.WriteString(line + "\n")
			debugFile.Sync()
		}
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

		// Protect against interleaved multi-session packet bleeding
		if expectedSessionID == "" && streamResp.ID != "" {
			expectedSessionID = streamResp.ID
		}
		if expectedSessionID != "" && streamResp.ID != expectedSessionID {
			continue
		}
		if len(streamResp.Choices) == 0 {
			continue
		}

		choice := streamResp.Choices[0]
		delta := choice.Delta

		// Handle explicit server finish signals safely
		if choice.FinishReason != "" {
			switch choice.FinishReason {
			case "tool_calls":
				serverSignaledStop = true
			case "stop":
				// If we have any tools gathered (JSON or text-fallback), halt cleanly
				if len(toolCallAccum) > 0 {
					serverSignaledStop = true
				}
			}
		}

		// 1. Unpack Thinking content (Handles raw XML parallel tool leakages)
		if delta.ReasoningContent != "" {
			rc := delta.ReasoningContent
			thinkingContent.WriteString(rc)
			rawTextBuffer.WriteString(rc)

			// Check if a raw text XML tool block has completed its declaration
			if strings.Contains(rawTextBuffer.String(), "</tool_call>") {
				parsedCalls := parseRawXmlTools(rawTextBuffer.String())
				for _, tc := range parsedCalls {
					// Map parallel text tools using a non-colliding key index offset
					key := 100 + tc.Index
					if _, exists := toolCallAccum[key]; !exists {
						localCopy := tc // Create local scope copy for map pointer preservation
						toolCallAccum[key] = &localCopy
						fmt.Printf("\n> 🔧 Planning tool call (Text-Fallback): %s\n", localCopy.Function.Name)
					}
				}
				// Find the position of the last closing tag we just processed
				lastCloseIdx := strings.LastIndex(rawTextBuffer.String(), "</tool_call>")
				if lastCloseIdx != -1 {
					// Keep everything that came AFTER the closing tag (like the next opening tag)
					remainingText := rawTextBuffer.String()[lastCloseIdx+len("</tool_call>"):]
					rawTextBuffer.Reset()
					rawTextBuffer.WriteString(remainingText)
				}
			}

			tokenCount := len(strings.Fields(rc))
			globalStats.TokenArrived(tokenCount)

			if config.ShowThinking {
				if !thinkingStarted {
					fmt.Print("\n> 🤔 Thinking...\n")
					thinkingStarted = true
				}
				fmt.Print(rc)
				os.Stdout.Sync()
			} else if !thinkingStarted {
				fmt.Print("\n> 🤔 Thinking hidden, run /showthink on to enable\n")
				thinkingStarted = true
			}
			continue
		}

		// Handle late finish triggers safely if the server flags stop state
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

		// 3. Unpack standard JSON Tool call deltas safely
		for _, tcDelta := range delta.ToolCalls {
			key := tcDelta.Index
			if _, exists := toolCallAccum[key]; !exists {
				toolCallAccum[key] = &ToolCall{Index: key}
			}
			tc := toolCallAccum[key]

			// Announce the JSON call as soon as the function name arrives
			if tcDelta.Function.Name != "" && tc.Function.Name == "" {
				fmt.Printf("\n> 🔧 Planning tool call (JSON): %s\n", tcDelta.Function.Name)
			}

			if tcDelta.ID != "" {
				tc.ID = tcDelta.ID
			}
			if tcDelta.Type != "" {
				tc.Type = tcDelta.Type
			}
			if tcDelta.Function.Name != "" {
				tc.Function.Name += tcDelta.Function.Name
			}
			tc.Function.Arguments += tcDelta.Function.Arguments
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

		// Perform validation check on completed strings
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

	// Trigger cutoff dumps only if the turn processed completely empty
	if len(toolCalls) == 0 && fullContent.Len() == 0 && thinkingContent.Len() == 0 {
		fmt.Fprintln(os.Stderr, "\n--- 🚨 STREAM CUTOFF DETECTED (LAST 10 LINES) ---")
		for i := 0; i < 10; i++ {
			line := debugHistory[(historyIdx+i)%10]
			if line != "" {
				if config.Debug && debugFile != nil {
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
