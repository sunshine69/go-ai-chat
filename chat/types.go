package main

import "time"

// https://github.com/hymkor/go-multiline-ny for multiline support TODO
type Config struct {
	BaseURL             string
	Model               string
	SummaryModel        string // model used for context summarisation; falls back to Model if empty
	SummaryModelTimeout string
	APIKey              string
	Timeout             time.Duration
	PromptedURL         bool
	PromptedModel       bool
	PromptedAPIKey      bool
	Debug               bool
	MCPPermissions      map[string]string
	ShowThinking        bool
	ContextLimit        int // max estimated tokens before trimming; 0 = disabled
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

type TextProcessor struct{}
type ImageProcessor struct{}

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
