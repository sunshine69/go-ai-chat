package main

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// Context trimming
// ---------------------------------------------------------------------------

// estimateTokens gives a fast token count estimate (chars/4 is the standard rule of thumb).
func estimateTokens(msgs []Message) int {
	total := 0
	for _, m := range msgs {
		switch v := m.Content.(type) {
		case string:
			total += len(v) / 4
		case []ContentPart:
			for _, p := range v {
				total += len(p.Text) / 4
			}
		}
		// Count tool call arguments too
		for _, tc := range m.ToolCalls {
			total += len(tc.Function.Arguments) / 4
		}
		total += 4 // per-message overhead (role tokens etc.)
	}
	return total
}

// trimContext compresses the oldest messages when the token budget is exceeded.
// It keeps:
//   - any leading system messages untouched
//   - the most recent 'keepTail' user/assistant turns verbatim (recent context is precious)
//   - everything in between is summarised into one system message via a sub-call to the AI
//
// If the sub-call fails it falls back to simply dropping the middle messages.
func trimContext(ctx context.Context, cfg Config, msgs []Message) []Message {
	const keepTail = 6

	systemHead := []Message{}
	rest := msgs
	for len(rest) > 0 && rest[0].Role == "system" {
		systemHead = append(systemHead, rest[0])
		rest = rest[1:]
	}

	if len(rest) <= keepTail+2 {
		return msgs
	}

	toCompress := rest[:len(rest)-keepTail]
	toKeep := rest[len(rest)-keepTail:]

	fmt.Println("✂️  Context too long — summarising older messages…")
	summary := summariseMessages(ctx, cfg, toCompress)

	summaryMsg := Message{
		Role:    "system",
		Content: "[Conversation summary — earlier messages compressed]\n" + summary,
	}

	trimmed := make([]Message, 0, len(systemHead)+1+len(toKeep))
	trimmed = append(trimmed, systemHead...)
	trimmed = append(trimmed, summaryMsg)
	trimmed = append(trimmed, toKeep...)

	after := estimateTokens(trimmed)
	target := cfg.ContextLimit / 2

	// Still over half the limit — aggressively drop tail messages until we're under target
	for after > target && len(toKeep) > 2 {
		toKeep = toKeep[2:] // drop oldest pair from tail
		trimmed = trimmed[:0]
		trimmed = append(trimmed, systemHead...)
		trimmed = append(trimmed, summaryMsg)
		trimmed = append(trimmed, toKeep...)
		after = estimateTokens(trimmed)
	}

	fmt.Printf("✅ Context trimmed (now ~%d tokens, target <%d)\n", after, target)
	return trimmed
}

// summariseMessages asks the model to produce a concise summary of a slice of messages.
// Falls back to a plain text concatenation if the API call fails.
func summariseMessages(ctx context.Context, cfg Config, msgs []Message) string {
	// Build a readable transcript to feed to the summariser.
	var transcript strings.Builder
	for _, m := range msgs {
		role := m.Role
		var text string
		switch v := m.Content.(type) {
		case string:
			text = v
		case []ContentPart:
			for _, p := range v {
				if p.Text != "" {
					text += p.Text + " "
				}
			}
		}
		if text == "" && len(m.ToolCalls) > 0 {
			names := make([]string, 0, len(m.ToolCalls))
			for _, tc := range m.ToolCalls {
				names = append(names, tc.Function.Name)
			}
			text = "[tool calls: " + strings.Join(names, ", ") + "]"
		}
		if text != "" {
			transcript.WriteString(fmt.Sprintf("%s: %s\n\n", role, strings.TrimSpace(text)))
		}
	}

	prompt := "The following is a conversation transcript. " +
		"Produce a concise but complete summary that preserves: " +
		"all decisions made, key facts established, file paths or code discussed, " +
		"tool calls and their outcomes, and any open questions. " +
		"Be dense — omit pleasantries only.\n\n" +
		transcript.String()

	summaryMsgs := []Message{
		{Role: "user", Content: prompt},
	}

	// Build a lightweight config for the summariser: same connection details
	// but swap in the dedicated summary model (if configured).
	summaryCfg := cfg
	if cfg.SummaryModel != "" {
		summaryCfg.Model = cfg.SummaryModel
	}
	// Disable context trimming for the sub-call to avoid recursion.
	summaryCfg.ContextLimit = 0
	// Suppress thinking output during background summarisation.
	summaryCfg.ShowThinking = false

	subCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	content, _, _, err := streamOnce(subCtx, summaryCfg, summaryMsgs)
	if err != nil || strings.TrimSpace(content) == "" {
		fmt.Println(" Fallback: plain truncated transcript")
		t := transcript.String()
		if len(t) > cfg.ContextLimit {
			t = t[:cfg.ContextLimit] + "\n…(truncated)"
		}
		return t
	}
	return content

}
