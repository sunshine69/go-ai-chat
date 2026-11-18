package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// The reasoning_effort field must reach the server only when /effort set it,
// so that "default" really means "let the model pick" (Strata: high).
func TestStreamOnceSendsReasoningEffort(t *testing.T) {
	var got map[string]interface{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &got)
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, `data: {"id":"1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"Task completed"},"finish_reason":""}]}`+"\n\n")
		io.WriteString(w, `data: {"id":"1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`+"\n\n")
		io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	base := Config{BaseURL: srv.URL, Model: "gyro-s", Timeout: 10 * time.Second, MaxTokens: 100, MaxRepeatPattern: 10, StallTimeout: 5 * time.Second}
	msgs := []Message{{Role: "user", Content: "hi"}}

	if _, _, _, err := streamOnce(context.Background(), base, msgs, true); err != nil {
		t.Fatal(err)
	}
	if _, ok := got["reasoning_effort"]; ok {
		t.Errorf("reasoning_effort must be omitted when unset, got %v", got["reasoning_effort"])
	}

	base.ReasoningEffort = "low"
	if _, _, _, err := streamOnce(context.Background(), base, msgs, true); err != nil {
		t.Fatal(err)
	}
	if got["reasoning_effort"] != "low" {
		t.Errorf("reasoning_effort = %v, want low", got["reasoning_effort"])
	}
	if !strings.Contains(srv.URL, "127.0.0.1") {
		t.Fatal("unexpected url")
	}
}
