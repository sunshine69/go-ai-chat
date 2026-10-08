package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

var statServerPort int = 9090

type Stats struct {
	// raw counters — written on every token chunk, read only on curl
	TotalTokens     atomic.Int64
	TotalRequests   atomic.Int64
	TotalErrors     atomic.Int64
	TotalDurationMs atomic.Int64 // sum of all generation durations
	TotalTTFTMs     atomic.Int64 // sum of all TTFT values

	// current in-flight request
	CurrentTokens     atomic.Int64
	CurrentStartMs    atomic.Int64 // unix ms when stream started
	CurrentFirstToken atomic.Int64 // unix ms of first token

	Uptime time.Time
}

var globalStats = &Stats{Uptime: time.Now()}

// Called at request start
func (s *Stats) StreamStarted() {
	s.CurrentTokens.Store(0)
	s.CurrentStartMs.Store(time.Now().UnixMilli())
	s.CurrentFirstToken.Store(0)
}

// Called on every arriving content token chunk — hot path, just atomics
func (s *Stats) TokenArrived(n int) {
	now := time.Now().UnixMilli()
	s.TotalTokens.Add(int64(n))
	s.CurrentTokens.Add(int64(n))
	// record first token time once
	if s.CurrentFirstToken.CompareAndSwap(0, now) {
		ttft := now - s.CurrentStartMs.Load()
		s.TotalTTFTMs.Add(ttft)
	}
}

// Called when stream finishes
func (s *Stats) StreamFinished() {
	first := s.CurrentFirstToken.Load()
	if first == 0 {
		return // no tokens arrived (tool-only response)
	}
	elapsed := time.Now().UnixMilli() - first
	s.TotalDurationMs.Add(elapsed)
	s.TotalRequests.Add(1)
}

func (s *Stats) RecordError() {
	s.TotalErrors.Add(1)
}

// All division happens here, only when someone curls
func (s *Stats) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	requests := s.TotalRequests.Load()
	tokens := s.TotalTokens.Load()
	durMs := s.TotalDurationMs.Load()
	ttftMs := s.TotalTTFTMs.Load()

	curTokens := s.CurrentTokens.Load()
	curStart := s.CurrentFirstToken.Load()

	// current in-flight tok/s
	var currentTokPerSec float64
	if curStart > 0 {
		elapsed := float64(time.Now().UnixMilli()-curStart) / 1000.0
		if elapsed > 0 {
			currentTokPerSec = float64(curTokens) / elapsed
		}
	}

	var avgTokPerSec, avgTTFT float64
	if requests > 0 && durMs > 0 {
		avgTokPerSec = float64(tokens) / (float64(durMs) / 1000.0)
		avgTTFT = float64(ttftMs) / float64(requests) / 1000.0
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"uptime_seconds":           time.Since(s.Uptime).Seconds(),
		"total_requests":           requests,
		"total_tokens":             tokens,
		"total_errors":             s.TotalErrors.Load(),
		"avg_tokens_per_sec":       fmt.Sprintf("%.1f", avgTokPerSec),
		"avg_ttft_sec":             fmt.Sprintf("%.2f", avgTTFT),
		"current_tokens":           curTokens,
		"current_tokens_per_sec":   fmt.Sprintf("%.1f", currentTokPerSec),
		"countSentencesWhenAILoop": countSentencesWhenAILoop,
		"randomSentenceCount":      randomSentenceCount,
		"randomSentence":           randomSentence,
	})
}

// ---------------------------------------------------------------------------
// SessionStats — per-period stats for the /stat, /reset start and /n commands.
// A "period" starts at program start (or the last /n or /reset start) and
// ends when the user runs /n or /reset start, which print the collected data
// and begin a fresh period.
// ---------------------------------------------------------------------------

type SessionStats struct {
	mu sync.Mutex

	StartTime time.Time // when the current period began

	ThinkingMs    int64     // total model thinking (reasoning) time
	AnswerMs      int64     // total model answer time
	Tokens        int64     // total generated tokens (thinking + answer)
	ToolCalls     int64     // total tool calls executed
	ToolSuccess   int64     // tool calls that returned a usable result
	ToolFailure   int64     // tool calls that failed (error / no MCP / denied)
	TurnDurations []float64 // per-model-turn tokens/sec samples
}

var sessionStats = &SessionStats{StartTime: time.Now()}

// NewSessionPeriod resets all counters and starts a fresh period now.
// The old period's data is returned so it can be printed before resetting.
func (s *SessionStats) NewSessionPeriod() *SessionStats {
	s.mu.Lock()
	defer s.mu.Unlock()
	old := &SessionStats{
		StartTime:     s.StartTime,
		ThinkingMs:    s.ThinkingMs,
		AnswerMs:      s.AnswerMs,
		Tokens:        s.Tokens,
		ToolCalls:     s.ToolCalls,
		ToolSuccess:   s.ToolSuccess,
		ToolFailure:   s.ToolFailure,
		TurnDurations: append([]float64(nil), s.TurnDurations...),
	}
	s.ThinkingMs = 0
	s.AnswerMs = 0
	s.Tokens = 0
	s.ToolCalls = 0
	s.ToolSuccess = 0
	s.ToolFailure = 0
	s.TurnDurations = nil
	s.StartTime = time.Now()
	return old
}

// RecordTurn finalizes one model turn: adds its thinking/answer time and
// generated tokens, and records its tokens/sec rate for min/avg/max.
func (s *SessionStats) RecordTurn(tokens, thinkingMs, answerMs int64) {
	s.mu.Lock()
	s.ThinkingMs += thinkingMs
	s.AnswerMs += answerMs
	s.Tokens += tokens
	if totalSec := float64(thinkingMs+answerMs) / 1000.0; totalSec > 0 && tokens > 0 {
		s.TurnDurations = append(s.TurnDurations, float64(tokens)/totalSec)
	}
	s.mu.Unlock()
}

// RecordToolCall records one executed tool call and whether it succeeded.
func (s *SessionStats) RecordToolCall(success bool) {
	s.mu.Lock()
	s.ToolCalls++
	if success {
		s.ToolSuccess++
	} else {
		s.ToolFailure++
	}
	s.mu.Unlock()
}

// Snapshot returns a copy of the current period for safe printing.
func (s *SessionStats) Snapshot() *SessionStats {
	s.mu.Lock()
	defer s.mu.Unlock()
	return &SessionStats{
		StartTime:     s.StartTime,
		ThinkingMs:    s.ThinkingMs,
		AnswerMs:      s.AnswerMs,
		Tokens:        s.Tokens,
		ToolCalls:     s.ToolCalls,
		ToolSuccess:   s.ToolSuccess,
		ToolFailure:   s.ToolFailure,
		TurnDurations: append([]float64(nil), s.TurnDurations...),
	}
}

// printSessionStats renders the given period snapshot to stderr.
func printSessionStats(s *SessionStats) {
	totalMs := s.ThinkingMs + s.AnswerMs
	var rate float64
	if s.ToolCalls > 0 {
		rate = float64(s.ToolSuccess) / float64(s.ToolCalls) * 100
	}

	var min, max, sum float64
	if len(s.TurnDurations) > 0 {
		min, max = s.TurnDurations[0], s.TurnDurations[0]
		for _, d := range s.TurnDurations {
			sum += d
			if d < min {
				min = d
			}
			if d > max {
				max = d
			}
		}
	}

	fmt.Fprintln(os.Stderr, "📊 Stats since "+s.StartTime.Format("2006-01-02 15:04:05")+" ("+time.Since(s.StartTime).Round(time.Second).String()+" ago):")
	fmt.Fprintf(os.Stderr, "   💭 Total model thinking time: %s\n", (time.Duration(s.ThinkingMs) * time.Millisecond).Round(time.Second))
	fmt.Fprintf(os.Stderr, "   💬 Total model answer time:   %s\n", (time.Duration(s.AnswerMs) * time.Millisecond).Round(time.Second))
	fmt.Fprintf(os.Stderr, "   ⏱️  Total model time:          %s\n", (time.Duration(totalMs) * time.Millisecond).Round(time.Second))
	fmt.Fprintf(os.Stderr, "   🔢 Total tokens generated:    %d\n", s.Tokens)
	fmt.Fprintf(os.Stderr, "   🔧 Tool calls:                %d (%d success, %d failed — %.0f%% success rate)\n",
		s.ToolCalls, s.ToolSuccess, s.ToolFailure, rate)
	if len(s.TurnDurations) > 0 {
		avg := sum / float64(len(s.TurnDurations))
		fmt.Fprintf(os.Stderr, "   🚀 Tok/sec (per turn):        min %.1f | avg %.1f | max %.1f\n", min, avg, max)
	} else {
		fmt.Fprintln(os.Stderr, "   🚀 Tok/sec (per turn):        n/a (no completed turns yet)")
	}
}

func StartStatsServer(port int) {
	mux := http.NewServeMux()
	mux.Handle("/stats", globalStats)
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprintln(w, "ok")
	})

	addr := fmt.Sprintf("127.0.0.1:%d", port)
	fmt.Fprintf(os.Stderr, "📡 Stats server listening on http://%s/stats\n", addr)

	go func() {
		if err := http.ListenAndServe(addr, mux); err != nil {
			fmt.Fprintf(os.Stderr, "⚠️  Stats server error: %v. Change the port if it is not available using env var STAT_SERVER_PORT\n", err)
		}
	}()
}
