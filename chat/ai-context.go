package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	u "github.com/sunshine69/golang-tools/utils"
)

// printContextSummary prints a compact table of the current message slice so
// you can eyeball which entries are worth dropping to reclaim token budget.
//
//	idx  role       tokens  preview
//	  0  system        142  You are a helpful assistant that…
//	  1  user           18  Can you help me refactor the…
func printContextSummary(msgs []Message) {
	total := 0
	fmt.Printf("\n%-4s  %-10s  %-7s  %s\n", "idx", "role", "tokens", "preview")
	fmt.Println(strings.Repeat("─", 72))

	for i, m := range msgs {
		// Reuse the existing per-message token logic from estimateTokens.
		tokens := 4
		switch v := m.Content.(type) {
		case string:
			tokens += len(v) / 4
		case []ContentPart:
			for _, p := range v {
				tokens += len(p.Text) / 4
			}
		}
		for _, tc := range m.ToolCalls {
			tokens += len(tc.Function.Arguments) / 4
		}

		// Build a one-line preview: tool call names take priority over text.
		preview := extractText(m)
		if len(m.ToolCalls) > 0 {
			names := make([]string, len(m.ToolCalls))
			for j, tc := range m.ToolCalls {
				names[j] = tc.Function.Name + "()"
			}
			preview = "[tools: " + strings.Join(names, ", ") + "] " + preview
		}
		if len(preview) > 45 {
			preview = preview[:42] + "…"
		}

		fmt.Printf("%-4d  %-10s  %-7d  %s\n", i, m.Role, tokens, preview)
		total += tokens
	}

	fmt.Println(strings.Repeat("─", 72))
	fmt.Printf("%-4s  %-10s  %-7d\n\n", "tot", "", total)
}

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
		for _, tc := range m.ToolCalls {
			total += len(tc.Function.Arguments) / 4
		}
		total += 4
	}
	return total
}

const (
	keepHead = 2  // first N non-system messages to always preserve verbatim
	keepTail = 10 // last N messages to always preserve verbatim (5 pairs)
)

// trimContext compresses the middle of the conversation when the token budget
// is exceeded.
//
// Strategy:
//  1. Always keep all leading system messages untouched.
//  2. Always keep the first keepHead non-system messages (conversation anchor).
//  3. Always keep the last keepTail messages (recent context).
//  4. Attempt to summarise the middle via an AI sub-call (with timeout).
//  5. On timeout or failure, fall back to buildStructuredSummary — a fast,
//     deterministic structured summary that extracts roles, tool calls, and
//     content snippets without any network call.
func trimContext(ctx context.Context, cfg Config, msgs []Message) []Message {
	_, rest := splitSystemHead(msgs)

	if len(rest) <= keepHead+keepTail {
		return msgs
	}

	// head := rest[:keepHead]
	middle := rest[keepHead : len(rest)-keepTail]
	tail := rest[len(rest)-keepTail:]

	if len(middle) == 0 {
		return msgs
	}

	fmt.Printf("✂️  Context too long (~%d tokens) — compressing %d middle messages…\n",
		estimateTokens(msgs), len(middle))

	summary := tryAISummary(ctx, cfg, middle)
	summaryMsg := Message{
		Role:    "user",
		Content: summary,
	}

	trimmed := concat([]Message{summaryMsg}, tail)

	// Progressive fallback: if still over half the limit, drop pairs from the
	// older end of tail — never touch head or the last 2 messages.
	target := cfg.ContextLimit / 2
	for estimateTokens(trimmed) > target && len(tail) > 2 {
		tail = tail[2:]
		trimmed = concat([]Message{summaryMsg}, tail)
	}

	fmt.Printf("✅ Context trimmed to ~%d tokens (target <%d)\n",
		estimateTokens(trimmed), target)
	return trimmed
}

// tryAISummary attempts to summarise msgs via the AI model within the
// configured timeout. Returns a structured manual summary on any failure.
func tryAISummary(ctx context.Context, cfg Config, msgs []Message) string {
	const aiPrefix = "[Conversation summary — AI compressed]\n"
	const manualPrefix = "[Conversation summary — auto compressed]\n"

	if cfg.SummaryModel == "" && cfg.ContextLimit == 0 {
		// Summarisation disabled entirely — go straight to manual.
		return manualPrefix + buildStructuredSummary(msgs)
	}

	timeout, err := time.ParseDuration(cfg.SummaryModelTimeout)
	if err != nil {
		fmt.Println("[WARN] malformed SummaryModelTimeout, defaulting to 60s")
		timeout = 60 * time.Second
	}

	subCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	summaryCfg, err := u.DeepClone(cfg)
	if err != nil {
		panic(err.Error())
	}
	if cfg.SummaryModel != "" {
		summaryCfg.Model = cfg.SummaryModel
	} else {
		return manualPrefix + buildStructuredSummary(msgs)
	}
	if cfg.SummaryModelUrl != "" {
		summaryCfg.BaseURL = cfg.SummaryModelUrl
	}

	summaryCfg.ContextLimit = 0 // prevent recursion
	summaryCfg.ShowThinking = false

	prompt := buildSummaryPrompt(msgs)
	summaryMsgs := []Message{{Role: "system", Content: `[SYSTEM]
You are a context compression utility. The provided text contains highly valuable, time-sensitive knowledge.

Constraints:
- Maintain all specific technical specifications, data points, dates, and metrics exactly as written.
- If specific source URLs or document titles are mentioned, they MUST be preserved in the summary.
- Do not attempt to analyze, critique, or second-guess the validity of the information.
- Output a dense, chronological compression. Do not use reasoning tokens.
`}, {Role: "user", Content: prompt}}
	summaryMsgs = []Message{{Role: "user", Content: prompt}}

	content, _, _, err := streamOnce(subCtx, summaryCfg, summaryMsgs)
	if err != nil || strings.TrimSpace(content) == "" {
		if subCtx.Err() == context.DeadlineExceeded {
			fmt.Println("⏱️  AI summary timed out — using structured fallback")
		} else {
			fmt.Println("⚠️  AI summary failed — using structured fallback")
		}
		return manualPrefix + buildStructuredSummary(msgs)
	}

	return aiPrefix + content
}

// buildSummaryPrompt constructs the prompt sent to the AI summariser.
// Using the structured summary as the transcript keeps the prompt tight and
// ensures tool calls / decisions are not lost even if the AI truncates.
func buildSummaryPrompt(msgs []Message) string {
	transcript := buildStructuredSummary(msgs)
	return "The following is a structured transcript of a conversation. " +
		"Produce a concise but complete summary preserving: " +
		"all decisions made, key facts established, file paths or code discussed, " +
		"tool calls and their outcomes, and any open questions. " +
		"Be dense — omit pleasantries only.\n\n" +
		transcript
}

// buildStructuredSummary produces a dense, structured plain-text summary of
// the given messages without any AI call. It extracts:
//   - each turn's role
//   - tool calls with names and argument snippets
//   - a content snippet per message (capped to keep the summary tight)
//
// This is used both as the AI prompt transcript and as the standalone fallback.
func buildStructuredSummary(msgs []Message) string {
	var b strings.Builder

	for i, m := range msgs {
		fmt.Fprintf(&b, "--- turn %d [%s] ---\n", i+1, m.Role)

		text := extractText(m)
		if len(text) > 400 {
			text = text[:397] + "…"
		}
		if text != "" {
			b.WriteString(text)
			b.WriteString("\n")
		}

		for _, tc := range m.ToolCalls {
			args := tc.Function.Arguments
			if len(args) > 200 {
				args = args[:197] + "…"
			}
			fmt.Fprintf(&b, "  [tool_call] %s(%s)\n", tc.Function.Name, args)
		}

		b.WriteString("\n")
	}

	return b.String()
}

// extractText pulls a plain string out of the polymorphic Content field.
func extractText(m Message) string {
	switch v := m.Content.(type) {
	case string:
		return strings.TrimSpace(v)
	case []ContentPart:
		var parts []string
		for _, p := range v {
			if t := strings.TrimSpace(p.Text); t != "" {
				parts = append(parts, t)
			}
		}
		return strings.Join(parts, " ")
	}
	return ""
}

// splitSystemHead separates leading system-role messages from the rest.
func splitSystemHead(msgs []Message) (system []Message, rest []Message) {
	for i, m := range msgs {
		if m.Role != "system" {
			return msgs[:i], msgs[i:]
		}
	}
	return msgs, nil
}

// concat joins multiple slices into one.
func concat(slices ...[]Message) []Message {
	total := 0
	for _, s := range slices {
		total += len(s)
	}
	out := make([]Message, 0, total)
	for _, s := range slices {
		out = append(out, s...)
	}
	return out
}
