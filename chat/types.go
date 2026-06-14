package main

import (
	"regexp"
	"time"
)

// Qwen response this and stop!
var aiRestResponsePatern = regexp.MustCompile(`Now I need to|I can|Let me|\<\/think\>`)

// https://github.com/hymkor/go-multiline-ny for multiline support TODO
type Config struct {
	BaseURL             string
	Model               string
	SummaryModel        string // model used for context summarisation; falls back to Model if empty
	SummaryModelTimeout string
	SummaryModelUrl     string
	APIKey              string
	Timeout             time.Duration
	PromptedURL         bool
	PromptedModel       bool
	PromptedAPIKey      bool
	Debug               bool
	DebugLevel          string
	MCPPermissions      map[string]string
	ShowThinking        bool
	ContextLimit        int    // max estimated tokens before trimming; 0 = disabled
	CtxOverSizeAllowed  int    // Within one turn, ctx may reach that number. If not set (0) then it is 2 * ContextLimit. Should be greater - this to serve as a burst in one session to allow ai has complete info during one turn.
	BlockedTools        string // coma sep of tools name we dont want AI to see and use
	MaxTokens           int
	MaxRepeatPattern    int
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

// OpenAIModelsResponse represents the top-level list container returned by the API.
type OpenAIModelsResponse struct {
	// Object string         `json:"object"`
	Data []ModelDetails `json:"data"`
}

// ModelDetails represents the extended configuration of an individual model.
type ModelDetails struct {
	ID string `json:"id"`
	// Aliases []string `json:"aliases"`
	// Tags    []string `json:"tags"`
	// Object  string   `json:"object"`
	// OwnedBy string   `json:"owned_by"`
	// Created        UnixTime     `json:"created"`
	Status       StatusInfo   `json:"status"`
	Architecture Architecture `json:"architecture"`
	// NeedDownload bool         `json:"need_download"`
}

// StatusInfo captures the current state, command-line arguments, and preset name.
type StatusInfo struct {
	// Value string `json:"value"`
	// Args   []string `json:"args"`
	Preset string `json:"preset"`
}

// Architecture specifies the media modalities supported by the system.
type Architecture struct {
	InputModalities  []string `json:"input_modalities"`
	OutputModalities []string `json:"output_modalities"`
}
