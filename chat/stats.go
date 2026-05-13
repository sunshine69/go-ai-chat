package main

import (
	"encoding/json"
	"fmt"
	"net/http"
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
		"uptime_seconds":         time.Since(s.Uptime).Seconds(),
		"total_requests":         requests,
		"total_tokens":           tokens,
		"total_errors":           s.TotalErrors.Load(),
		"avg_tokens_per_sec":     fmt.Sprintf("%.1f", avgTokPerSec),
		"avg_ttft_sec":           fmt.Sprintf("%.2f", avgTTFT),
		"current_tokens":         curTokens,
		"current_tokens_per_sec": fmt.Sprintf("%.1f", currentTokPerSec),
	})
}

func StartStatsServer(port int) {
	mux := http.NewServeMux()
	mux.Handle("/stats", globalStats)
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprintln(w, "ok")
	})

	addr := fmt.Sprintf("127.0.0.1:%d", port)
	fmt.Printf("📡 Stats server listening on http://%s/stats\n", addr)

	go func() {
		if err := http.ListenAndServe(addr, mux); err != nil {
			fmt.Printf("⚠️  Stats server error: %v. Change the port if it is not available using env var STAT_SERVER_PORT\n", err)
		}
	}()
}
