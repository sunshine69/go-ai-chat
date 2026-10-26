package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"time"
)

// Sentinel errors for context overflow. askAI reacts to these by trimming
// the context and retrying the request.
var (
	errContextExceeded = errors.New("CONTEXT SIZE EXCEEDED") // backend rejected the prompt as too big
	errOutputTruncated = errors.New("OUTPUT TRUNCATED")      // backend ran out of context mid-generation
)

// lastRequestWasTruncated feeds the proactive reactive-trim check at the top
// of the askAI loop for the ContextLimit == 0 case.
var lastRequestWasTruncated bool

// dropLeadingToolMsgs drops leading tool-result messages whose matching
// assistant tool_call message was not preserved (e.g. it was summarised
// away). Orphan tool messages make the API reject the whole payload.
func dropLeadingToolMsgs(msgs []Message) []Message {
	i := 0
	for i < len(msgs) && msgs[i].Role == "tool" {
		i++
	}
	if i == 0 {
		return msgs
	}
	fmt.Fprintf(os.Stderr, "⚠️  Dropping %d orphan tool-result message(s) at the head of the kept context\n", i)
	return msgs[i:]
}

// askAI — streams the response; if MCP is active, injects tools and handles
// tool_calls returned by the model in a loop until the model gives a final answer.
func askAI(ctx context.Context, config Config, msgs []Message) (string, string, []Message, error) {
	// Build a local copy of history for the tool loop — we extend it as we call tools
	workingMsgs := make([]Message, len(msgs))
	copy(workingMsgs, msgs)
	var accumulatedContent strings.Builder
	var accumulatedThinking strings.Builder
	foundAndExecToolCall := false
	repeatedPatternCount = 0
	// Looping tools calls within 1 turn: if the model returns a tool_call, we execute it and feed the result back to the model until it returns a final answer (no tool calls).
	// We also detect and handle various failure modes (stall, cutoff, thinking loops) and nudge the model to continue or change its approach by crafting a user message that tells it to continue or change its thinking.
	for {
		currentEstToken := estimateTokens(workingMsgs)
		// If we found tools call we dont check the context to avoid losing information
		// but it still needs to be smaller than ctxOverSizeAllowed.
		if config.ContextLimit > 0 && (!foundAndExecToolCall || currentEstToken >= config.CtxOverSizeAllowed) {
			if currentEstToken >= config.ContextLimit {
				fmt.Fprintf(os.Stderr, "📊 Context size: ~%d tokens (limit: %d). Start triming context ... ", estimateTokens(workingMsgs), config.ContextLimit)
				workingMsgs = trimContext(ctx, config, workingMsgs)
				fmt.Fprintln(os.Stderr, "done")
			}
		} else if config.ContextLimit == 0 && len(workingMsgs) > 0 && lastRequestWasTruncated {
			// Reactive trim: the previous request ran out of context (server
			// truncation or exceed_context_size error) even though no explicit
			// limit is configured. Trim once to make room before retrying.
			lastRequestWasTruncated = false
			fmt.Fprintln(os.Stderr, "📊 Context overflow detected (no /ctx limit set) — trimming context ... ")
			workingMsgs = trimContext(ctx, config, workingMsgs)
			fmt.Fprintln(os.Stderr, "done")
		}
		// Timestamp each AI model turn so the user can see how long each
		// round-trip took (thinking → response → tool execution → response).
		log.Printf("━━━ AI turn start ━━━")

		content, thinking, toolCalls, err := streamOnce(ctx, config, workingMsgs, false)

		// If streamOnce failed with a recoverable error (nudge-loop, cutoff…)
		// we still want a final timestamp on the last turn it produced.
		finalizeTurn := func() { log.Printf("━━━ AI turn end   ━━━") }

		var collectContentAndThink = func() {
			// Always gather whatever text it managed to output
			if content != "" {
				accumulatedContent.WriteString(content)
			}
			if thinking != "" {
				accumulatedThinking.WriteString(thinking)
			}
		}
		var remindAiFunc = func(msg string) {
			collectContentAndThink()
			finalizeTurn()
			workingMsgs = append(workingMsgs, Message{
				Role:    "assistant",
				Content: thinking,
			})
			// Add a firm nudge telling it to execute step 1 immediately
			workingMsgs = append(workingMsgs, Message{
				Role:    "user",
				Content: msg,
			})
		}
		if err != nil {
			errMsg := err.Error()
			needTrimCtx := (errors.Is(err, errContextExceeded) || errors.Is(err, errOutputTruncated))
			fmt.Fprintf(os.Stderr, "\n> ⚠️  [DEBUG] Stream error: %s - Need trim context: %t\n", errMsg, needTrimCtx)
			// needTrimCtx = needTrimCtx || (strings.Contains(errMsg, "exceed_context_size") || strings.Contains(errMsg, "exceeds the available context size"))
			if ctx.Err() == nil || needTrimCtx {
				// Context overflow: the request was too big for the backend.
				// Trim and retry — up to 2 times, and only while we haven't
				// accumulated a final answer yet.
				fmt.Fprintf(os.Stderr, "\n> ⚠️  [DEBUG] Get inside. ctx.Err: %v\n", ctx.Err())
				if needTrimCtx {
					fmt.Fprintln(os.Stderr, "\n> 🔁 [System Nudge]: Stream stalled mid-generation due to context nearly full — reconnecting and resuming...")
					remindAiFunc("Your context size is nearly full causing instability. Carefull when reading large files or thinking for too long. Need more action. Continue.")
					continue
				}
				switch errMsg {
				case "STREAM CUTOFF DETECTED", "AI HAS BECOME MENTAL":
					remindAiFunc("continue. If completed replied with string 'Task completed'")
					continue
				case "AI STREAM STALLED":
					// Mid-generation hang (no bytes for stallTimeout) — the connection
					// has already been force-closed by streamOnce's watchdog. Nudge the
					// model to resume from where it left off rather than restart.
					fmt.Fprintln(os.Stderr, "\n> 🔁 [System Nudge]: Stream stalled mid-generation — reconnecting and resuming...")
					remindAiFunc("Your previous response stalled mid-generation and the connection was cut. Resume exactly from where you left off — do not repeat what you already said and do not restart the task. If it was already complete, reply 'Task completed'.")
					continue
				case "AI HAS STUCK LOOP":
					if repeatedPatternCount > config.MaxRepeatPattern {
						repeatedPatternCount = 0
						remindAiFunc("continue. You got into thinking LOOP. Get it out and TRY something else!")
					}
					continue
				default:
					fmt.Fprintf(os.Stderr, "\n> 🔁 [WARN] Does it match any of the case. errMsg: '%s'\n", errMsg)
					finalizeTurn()
					return accumulatedContent.String() + content, accumulatedThinking.String() + thinking, workingMsgs, err
				}
			} else {
				fmt.Fprintf(os.Stderr, "\n> ⚠️  [DEBUG] Context canceled by user/system: %v\n", ctx.Err())
			}
			finalizeTurn()
			return accumulatedContent.String() + content, accumulatedThinking.String() + thinking, workingMsgs, err
		}
		collectContentAndThink()
		// --- 🛌 THE LAZY MODEL AUTO-NUDGE TRAP HAHA ---
		// If it chose to "stop" naturally, but left you with ZERO tools and ZERO text. No error return
		// after doing a bunch of thinking, it's being lazy. Wake it up!
		// sentences, _ := getSentencesAtPosition(content, "-2,-1")
		if !config.AutoNudgeDisabled && len(toolCalls) == 0 && !strings.Contains(content, "Task completed") && strings.Contains(content, "</think>") {
			fmt.Fprintln(os.Stderr, "\n> ⚡ [System Nudge]: LOOP - Forcing execution in 5 secs...")
			time.Sleep(5 * time.Second)
			remindAiFunc("You need to STOP and CHANGE thought to get out of loop. START action.")
			continue
		}
		// Thinking loop
		if !config.AutoNudgeDisabled && randomSentenceCount > config.MaxRepeatPattern {
			randomSentence = ""
			randomSentenceCount = 0
			fmt.Fprintf(os.Stderr, "\n> ⚡ [System Nudge]: Repeated one sentence %s. Forcing execution in 5 secs...\n", randomSentence)
			time.Sleep(5 * time.Second)
			remindAiFunc("You repeat loop. GET OUT OF LOOP, TRYING SOMETHING ELSE.")
			continue
		}

		// content too short (less than 3 lines ~ 300) is also a sign of drop
		if !config.AutoNudgeDisabled && ctx.Err() == nil && !strings.Contains(content, "Task completed") && !strings.Contains(content, "has been successfully") && len(toolCalls) == 0 && (len(content) < 600 && thinking != "") {
			fmt.Fprintln(os.Stderr, "\n> ⚡ [System Nudge]: SLEEP after planning. Forcing execution in 5 secs...")
			time.Sleep(5 * time.Second)
			remindAiFunc("Continue. If completed replied with string 'Task completed'")
			continue
		}
		// No tool calls — we're done
		if len(toolCalls) == 0 {
			finalizeTurn()
			return accumulatedContent.String(), accumulatedThinking.String(), workingMsgs, nil
		} else {
			foundAndExecToolCall = false // reset it here
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
				fmt.Fprintf(os.Stderr, "\n🚫 Permission Denied: Tool '%s' is blocked.\n", tc.Function.Name)
				allowed = false
			case "ask":
				fmt.Fprintf(os.Stderr, "\n⚠️  Tool '%s' requires permission.\n", tc.Function.Name)
				fmt.Fprintf(os.Stderr, "   Arguments:\n%s\n", argsDisplay) // Print arguments BEFORE asking
				fmt.Fprintf(os.Stderr, "   Allow? [y/N/auto]: ")

				var response string
				// Use Scanln to wait for user input
				fmt.Scanln(&response)
				if strings.ToLower(response) != "y" {
					fmt.Fprintf(os.Stderr, "❌ User denied permission for '%s'.\n", tc.Function.Name)
					allowed = false
				}
				if response == "auto" {
					fmt.Fprintf(os.Stderr, "✅ User granted permission for all future calls to '%s'.\n", tc.Function.Name)
					// Update the config to allow this tool in future calls
					config.MCPPermissions[tc.Function.Name] = "auto"
				}
			}

			// --- 3. EXECUTION ---
			var toolResult string
			if !allowed {
				toolResult = fmt.Sprintf("error: permission denied for tool %s (policy: %s)", tc.Function.Name, perm)
			} else {
				fmt.Fprintf(os.Stderr, "\n🔧 Calling tool: %s\n", tc.Function.Name)

				// We already parsed argsMap above, let's reuse it or re-parse for the actual call
				var finalArgs map[string]interface{}
				if err := json.Unmarshal([]byte(tc.Function.Arguments), &finalArgs); err != nil {
					finalArgs = map[string]interface{}{}
				}

				if activeMCP != nil {
					toolResult, err = activeMCP.CallTool(tc.Function.Name, finalArgs)
					if err != nil {
						toolResult = fmt.Sprintf("error: %v", err)
						fmt.Fprintf(os.Stderr, "   ❌ Tool error: %v\n", err)
						fmt.Fprintf(os.Stderr, "   💡 Tip: run /mcp schema to check expected argument names\n")
					} else {
						if config.Debug {
							fmt.Fprintf(os.Stderr, "   ✅ result: %s\n", toolResult)
						}
						foundAndExecToolCall = true
					}
				} else {
					toolResult = "error: no MCP client connected"
				}
				fmt.Fprintln(os.Stderr, "\n✅ Calling tool: done")
			}
			// Record the outcome for /stat — every failure path above sets the
			// "error: ..." prefix on toolResult.
			sessionStats.RecordToolCall(!strings.HasPrefix(toolResult, "error:"))

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
// (content, thinking, toolCalls, error). Tool call if detected and parsed ok will end the turn.
// quiet suppresses console output and MCP tool injection — used by the
// context-compression sub-call so a summary cannot hijack the tool loop or
// spam the user's terminal mid-conversation.
func streamOnce(ctx context.Context, config Config, msgs []Message, quiet bool) (string, string, []ToolCall, error) {
	globalStats.StreamStarted()
	defer globalStats.StreamFinished()

	streamCtx, cancelStream := context.WithCancel(ctx)
	defer cancelStream() // Clean up resources when function exits

	reqBody := map[string]interface{}{
		"model":      config.Model,
		"max_tokens": config.MaxTokens,
		"messages":   msgs,
		"stream":     true,
	}

	if config.Temperature > 0 {
		reqBody["temperature"] = config.Temperature
	}
	if config.TopP > 0 {
		reqBody["top_p"] = config.TopP
	}
	if config.TopK > 0 {
		reqBody["top_k"] = config.TopK
	}
	if config.MinP > 0 {
		reqBody["min_p"] = config.MinP
	}

	// Inject MCP tools if connected.
	// QUIET calls (e.g. the context-compression sub-call) must not get
	// tools: a summarisation answer that turns into a tool call has nowhere
	// to go and would silently fail the summary.
	if !quiet && activeMCP != nil && len(activeMCP.Tools()) > 0 {
		visibleTools := parseAndFilterToolsRegex(activeMCP.Tools(), config.BlockedTools)
		if len(visibleTools) > 0 {
			reqBody["tools"] = ToOpenAITools(visibleTools)
			reqBody["tool_choice"] = "auto"
		}
	}

	jsonValue, _ := json.Marshal(reqBody)
	client := &http.Client{Timeout: config.Timeout}

	req, err := http.NewRequestWithContext(streamCtx, "POST", config.BaseURL, bytes.NewBuffer(jsonValue))
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
		content, thinking, toolCalls, err := HandleAPIErrorFromLlamaServer(resp.StatusCode, string(body))
		// Mark context overflow distinctly so askAI can trim and retry.
		if err != nil && (strings.Contains(err.Error(), "exceed_context_size") || strings.Contains(err.Error(), "exceeds the available context size")) {
			return content, thinking, toolCalls, fmt.Errorf("%w: %s", errContextExceeded, err.Error())
		}
		return content, thinking, toolCalls, err
	}

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 4194304), 4194304) // 4Mb large enough for one turn

	var fullContent strings.Builder
	var thinkingContent strings.Builder
	var rawTextBuffer strings.Builder
	var thinkingStarted = false
	var headerPrinted = false
	var serverSignaledStop = false
	var serverOutputTruncated = false // server hit its output/context cap (finish_reason "length")

	// Per-turn tracking for /stat — the elapsed time between arriving chunks
	// is attributed to thinking or answer generation depending on which kind
	// of chunk arrived next.
	var turnThinkingMs int64
	var turnAnswerMs int64
	var turnTokens int64
	var turnLastChunkAt time.Time
	var turnHasChunk bool
	recordChunk := func(kind string, n int) {
		now := time.Now()
		if turnHasChunk {
			elapsed := int64(now.Sub(turnLastChunkAt) / time.Millisecond)
			if kind == "thinking" {
				turnThinkingMs += elapsed
			} else {
				turnAnswerMs += elapsed
			}
		}
		turnTokens += int64(n)
		turnLastChunkAt = now
		turnHasChunk = true
	}
	defer func() {
		if turnHasChunk {
			sessionStats.RecordTurn(turnTokens, turnThinkingMs, turnAnswerMs)
		}
	}()

	// Accumulate tool calls across streaming chunks (indexed by tool call index)
	toolCallAccum := map[int]*ToolCall{}

	// --- Mid-stream stall watchdog ---
	// Some backends (notably MoE / "expert-routed" models when the router
	// misfires) stop emitting SSE bytes entirely mid-generation: no error,
	// no [DONE], no TCP reset — scanner.Scan() just blocks forever.
	// config.Timeout bounds the *whole* request and doesn't help here (a
	// long legitimate generation shouldn't be killed just for being long).
	// What we need is an *inactivity* timeout: if no bytes arrive for
	// stallTimeout, treat the connection as hung, close it to unblock the
	// scanner, and let askAI() send a resume nudge on a fresh request.
	stallTimeout := config.StallTimeout
	if stallTimeout <= 0 {
		stallTimeout = 45 * time.Second
	}
	var lastActivity atomic.Int64
	lastActivity.Store(time.Now().UnixNano())
	var stalled atomic.Bool
	stopWatchdog := make(chan struct{})
	defer close(stopWatchdog)

	go func() {
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-stopWatchdog:
				return
			case <-streamCtx.Done():
				return
			case <-ticker.C:
				last := time.Unix(0, lastActivity.Load())
				if time.Since(last) >= stallTimeout {
					stalled.Store(true)
					fmt.Fprintf(os.Stderr, "\n> ⚠️  [Watchdog] No stream activity for %s — cancelling request and forcing recovery...\n", stallTimeout)
					cancelStream()
					return
				}
			}
		}
	}()

	debugHistory := make([]string, 10)
	historyIdx := 0
	var expectedSessionID string

	for scanner.Scan() {
		// Any successful read — even a keepalive/ping line — counts as activity.
		lastActivity.Store(time.Now().UnixNano())

		if streamCtx.Err() != nil {
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
			case "length":
				// Backend hit its max output / remaining context cap — the
				// response was cut off mid-generation. Flag it so the caller
				// can trim the context and retry instead of accepting a
				// truncated answer as final.
				serverOutputTruncated = true
			}
		}

		// 1. Unpack Thinking content (Handles raw XML parallel tool leakages)
		if delta.ReasoningContent != "" {
			rc := delta.ReasoningContent
			thinkingContent.WriteString(rc)
			rawTextBuffer.WriteString(rc)

			if !quiet { // loop detection only for real conversation turns, not summary sub-calls
				for _, s := range sentencesWhenAILoop {
					if strings.Contains(fullContent.String(), s) {
						countSentencesWhenAILoop++
						if !config.AutoNudgeDisabled && countSentencesWhenAILoop > 10 {
							countSentencesWhenAILoop = 0
							return fullContent.String(), thinkingContent.String(), []ToolCall{}, fmt.Errorf("AI HAS STUCK LOOP")
						}
					}
				}
				if len(fullContent.String()) > 600 { // Only collect if this is is large enough to form a paragraph
					if aiRestResponsePtn.MatchString(fullContent.String()) {
						repeatedPatternCount++
					}
					if randomSentence == "" { // looks like first sentence in a para. is most repeated
						randomSentences, _ := getSentencesAtPosition(fullContent.String(), "0")
						if len(randomSentences) > 0 {
							randomSentence = randomSentences[0]
						}
					} else {
						if strings.Contains(fullContent.String(), randomSentence) {
							randomSentenceCount++
						}
					}
					if repeatedPatternCount > config.MaxRepeatPattern {
						if !config.AutoNudgeDisabled {
							return fullContent.String(), thinkingContent.String(), []ToolCall{}, fmt.Errorf("AI HAS STUCK LOOP")
						}
					}
					if randomSentenceCount > config.MaxRepeatPattern {
						if !config.AutoNudgeDisabled {
							return fullContent.String(), thinkingContent.String(), []ToolCall{}, fmt.Errorf("AI HAS STUCK LOOP")
						}
					}
				}
			}
			// Check if a raw text XML tool block has completed its declaration
			if strings.Contains(rawTextBuffer.String(), "</tool_call>") {
				parsedCalls := parseRawXmlTools(rawTextBuffer.String())
				for _, tc := range parsedCalls {
					// Map parallel text tools using a non-colliding key index offset
					key := 100 + tc.Index
					if _, exists := toolCallAccum[key]; !exists {
						localCopy := tc // Create local scope copy for map pointer preservation
						toolCallAccum[key] = &localCopy
						fmt.Fprintf(os.Stderr, "\n> 🔧 Planning tool call (Text-Fallback): %s\n", localCopy.Function.Name)
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
			recordChunk("thinking", tokenCount)

			if !quiet && aiThoughtBecomeMentalPtn.MatchString(rc) {
				if !config.AutoNudgeDisabled {
					return fullContent.String(), thinkingContent.String(), []ToolCall{}, fmt.Errorf("AI HAS BECOME MENTAL")
				}
			}
			if !quiet {
				if config.ShowThinking {
					if !thinkingStarted {
						log.Print("\n> 🤔 Thinking...\n")
						thinkingStarted = true
					}
					fmt.Fprint(os.Stdout, rc)
					os.Stdout.Sync()
				} else if !thinkingStarted {
					log.Print("\n> 🤔 Thinking hidden, run /showthink on to enable\n")
					thinkingStarted = true
				}
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
			recordChunk("answer", tokenCount)

			if !quiet {
				if !headerPrinted {
					log.Print("\n> 📝 Response:\n")
					headerPrinted = true
				}
				fmt.Fprint(os.Stdout, delta.Content)
				os.Stdout.Sync()
			}
			fullContent.WriteString(delta.Content)

			if !quiet {
				if aiRestResponsePtn.MatchString(delta.Content) {
					repeatedPatternCount++
				}
				if repeatedPatternCount > config.MaxRepeatPattern {
					break
				}
			}
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
				log.Printf("\n> 🔧 Planning tool call (JSON): %s\n", tcDelta.Function.Name)
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
			// Capture Gemini thought_signature — treat as opaque blob, don't concatenate
			if tcDelta.ExtraContent != nil && tcDelta.ExtraContent.Google != nil {
				sig := tcDelta.ExtraContent.Google.ThoughtSignature
				if sig != "" {
					if tc.ExtraContent == nil {
						tc.ExtraContent = &ExtraContent{Google: &GoogleExtraContent{}}
					}
					tc.ExtraContent.Google.ThoughtSignature = sig // overwrite, not append
				}
			}
		}
	} // scanner end

	// 1. Explicitly check your atomic watchdog flag first
	if stalled.Load() {
		return fullContent.String(), thinkingContent.String(), nil, fmt.Errorf("AI STREAM STALLED")
	}

	// 2. Check if the parent context was canceled by the user/system
	if ctx.Err() != nil {
		return fullContent.String(), thinkingContent.String(), nil, ctx.Err()
	}

	// 3. Catch generic network errors (unrelated to your watchdog)
	if err := scanner.Err(); err != nil {
		log.Printf("[ERR] scanner error %s\n", err.Error())
		return fullContent.String(), thinkingContent.String(), nil, fmt.Errorf("stream error: %v", err)
	}

	// Collect accumulated tool calls safely using a range loop over the map
	var toolCalls []ToolCall
	for _, tc := range toolCallAccum {
		if tc == nil || tc.Type != "function" || strings.TrimSpace(tc.Function.Name) == "" {
			log.Printf("⚠️ Invalid - Type %s - Funcname %s\n ", tc.Type, tc.Function.Name)
			continue
		}

		args := strings.TrimSpace(tc.Function.Arguments)
		if args == "" {
			fmt.Fprintf(os.Stderr, "\n> ⚠️ Ignoring incomplete tool call (missing args): %s\n", tc.Function.Name)
			continue
		}

		// Perform validation check on completed strings
		var tmp interface{}
		if err := json.Unmarshal([]byte(args), &tmp); err != nil {
			fmt.Fprintf(os.Stderr, "\n> ⚠️ Ignoring malformed tool call %s: %v\n", tc.Function.Name, err)
			continue
		}

		if config.Debug {
			log.Printf("CALL TOOL: %+v\n", tc)
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
		return fullContent.String(), thinkingContent.String(), toolCalls, fmt.Errorf("STREAM CUTOFF DETECTED")
	}
	// The server cut the output short (context nearly full): report it as a
	// distinct error so askAI trims the context and retries, instead of
	// accepting a half-finished answer as the final one.
	if serverOutputTruncated && len(toolCalls) == 0 {
		fmt.Fprintln(os.Stderr, "\n> 📊 [Watchdog] Output truncated by backend (context nearly full) — flagging for context trim")
		return fullContent.String(), thinkingContent.String(), toolCalls, errOutputTruncated
	}
	return fullContent.String(), thinkingContent.String(), toolCalls, nil
}

// This func will handle and automate some actions when llama-server return messages
func HandleAPIErrorFromLlamaServer(statusCode int, body string) (string, string, []ToolCall, error) {
	/* {"error":{"code":400,"message":"request (132126 tokens) exceeds the available context size (132096 tokens), try increasing it","type":"exceed_context_size_error", n_prompt_tokens":132126,"n_ctx":132096}}
	 */
	return "", "", nil, fmt.Errorf("API Error: %s", body)
}
