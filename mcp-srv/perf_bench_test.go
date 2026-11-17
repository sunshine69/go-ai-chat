package main

// Cost of the debug tracer and of runCapped, measured rather than assumed.
// Run: go test ./mcp-srv/ -run X -bench . -benchmem

import (
	"context"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }

// plainRun is what the tools did before: capture into a builder, unbounded Run().
func plainRun(b *testing.B, cmdLine string) {
	for i := 0; i < b.N; i++ {
		var out, err strings.Builder
		cmd := exec.Command("sh", "-c", cmdLine)
		cmd.Stdout, cmd.Stderr = &out, &err
		if e := cmd.Run(); e != nil {
			b.Fatal(e)
		}
	}
}

func BenchmarkRun_plain_echo(b *testing.B)  { plainRun(b, "echo hi") }
func BenchmarkRun_capped_echo(b *testing.B) { cappedRun(b, "echo hi") }

// cappedRun is the shipped path: process group + WaitDelay + bounded ctx +
// wait goroutine.
func cappedRun(b *testing.B, cmdLine string) {
	prevD, prevM := execWaitDelay, execMaxRun
	execWaitDelay, execMaxRun = 2*time.Second, 10*time.Minute
	defer func() { execWaitDelay, execMaxRun = prevD, prevM }()
	for i := 0; i < b.N; i++ {
		var out, err strings.Builder
		cmd := exec.Command("sh", "-c", cmdLine)
		cmd.Stdout, cmd.Stderr = &out, &err
		if e := runCapped(context.Background(), cmd); e != nil {
			b.Fatal(e)
		}
	}
}

func BenchmarkRun_capped_bgchild(b *testing.B) {
	// The case the fix exists for: child outlives the shell.
	prevD, prevM := execWaitDelay, execMaxRun
	execWaitDelay, execMaxRun = 200*time.Millisecond, 10*time.Minute
	defer func() { execWaitDelay, execMaxRun = prevD, prevM }()
	for i := 0; i < b.N; i++ {
		var out, err strings.Builder
		cmd := exec.Command("sh", "-c", "sleep 5 & echo started")
		cmd.Stdout, cmd.Stderr = &out, &err
		if e := runCapped(context.Background(), cmd); e != nil {
			b.Fatal(e)
		}
		if !strings.Contains(out.String(), "started") {
			b.Fatal("lost output")
		}
	}
}

func BenchmarkDbg_disabled(b *testing.B) {
	old := dbgOn
	dbgOn = false
	defer func() { dbgOn = old }()
	for i := 0; i < b.N; i++ {
		dbg("http-in #%d POST /mcp/path1 remote=%s body=%q", i, "127.0.0.1:1234", "{...}")
	}
}

func BenchmarkDbg_enabled(b *testing.B) {
	old, oldOut := dbgOn, dbgOut
	dbgOn, dbgOut = true, discardWriter{}
	defer func() { dbgOn, dbgOut = old, oldOut }()
	for i := 0; i < b.N; i++ {
		dbg("http-in #%d POST /mcp/path1 remote=%s body=%q", i, "127.0.0.1:1234", "{...}")
	}
}

func BenchmarkFlightTrack(b *testing.B) {
	for i := 0; i < b.N; i++ {
		flightAdd("id-1", "tools/call", "sess-1", "path1")
		flightDel("id-1", "sess-1")
	}
}

func BenchmarkSnapshot(b *testing.B) {
	flightAdd("id-1", "tools/call", "sess-1", "path1")
	defer flightDel("id-1", "sess-1")
	for i := 0; i < b.N; i++ {
		_ = dbgFlow.snapshot()
	}
}

type noopHandler struct{}

func (noopHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) }

type noopResponseWriter struct{ h http.Header }

func (w *noopResponseWriter) Header() http.Header      { return w.h }
func (w *noopResponseWriter) Write(p []byte) (int, error) { return len(p), nil }
func (w *noopResponseWriter) WriteHeader(int)          {}
func (w *noopResponseWriter) Flush()                   {}

func httpNoopHandler() http.Handler { return noopHandler{} }

func newRequest() (*http.Request, error) {
	return http.NewRequest("POST", "/mcp/path1", strings.NewReader("{}"))
}

func statusWrap() http.ResponseWriter { return &statusRecorder{ResponseWriter: &noopResponseWriter{h: http.Header{}}} }

func BenchmarkDbgHTTP_passthrough(b *testing.B) {
	// With the tracer off, DbgHTTP must add literally nothing.
	old := dbgOn
	dbgOn = false
	defer func() { dbgOn = old }()
	h := DbgHTTP(httpNoopHandler())
	req, _ := newRequest()
	for i := 0; i < b.N; i++ {
		h.ServeHTTP(statusWrap(), req)
	}
}

func BenchmarkEnvDurationStartup(b *testing.B) {
	os.Setenv("AIG_EXEC_TIMEOUT", "10m")
	for i := 0; i < b.N; i++ {
		_ = envDurationOr("AIG_EXEC_TIMEOUT", time.Minute)
	}
}
