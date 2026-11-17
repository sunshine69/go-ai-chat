package main

// Debug tracer for the streamable-HTTP "two clients, one hangs" investigation.
//
// Enable with AIG_DEBUG=1. Every record goes to stderr — the same channel the
// audit log already uses — so one grep over the server log reconstructs the
// timeline of a hang that left you no log the first time.
//
// The records, in the order a hang reveals itself:
//
//   |DBG|req-start|   every JSON-RPC request the server accepts: MCP request id,
//                     method, Mcp-Session-Id, and the /mcp/<workdir> path. If a
//                     request appears here and its reply never does, the server
//                     took it and never answered.
//   |DBG|req-end|     same id, with elapsed time. Matching start/end ids tells
//                     you immediately whether the stall is server-side (no end)
//                     or client-side (end present, client still waiting).
//   |DBG|handler-*|   tool-handler level, so "handler never returned" separates
//                     from "handler returned but the response never reached the
//                     client" (the streaming/SSE leg).
//   |DBG|http-in/out| transport level: request line accepted, plus the status
//                     code and byte count of the response we wrote. A request
//                     with an http-in and no http-out is stuck inside the
//                     handler chain; no http-in at all means net/http never
//                     dispatched it (accept/backlog/connection pool).
//   |DBG|wm|          WorkdirManager acquire — who holds which workdir, and the
//                     409 "already occupied" path.
//   |DBG|sig|         every signal the process receives. Answers "did -15 reach
//                     the server at all, or did it hit the launcher".
//   |DBG|exec-*|      around cmd.Run() in the command tools: pid, elapsed wait.
//                     A long post with no live child means we were blocked
//                     reaping or draining, not running.
//   |DBG|dump|        goroutine stacks + process/children/fd state, fired by
//                     signal (AIG_DEBUG_SIG, default 3=SIGQUIT, i.e. kill -QUIT)
//                     and automatically when a request exceeds
//                     AIG_DEBUG_STALL (default 20s). The automatic dump is the
//                     one that matters: it lands in the log by itself, with no
//                     chance needed to intervene.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	mcp "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

const dbgPrefix = "|DBG|"

var (
	dbgOn    = os.Getenv("AIG_DEBUG") == "1"
	dbgMu    sync.Mutex
	dbgOut   io.Writer = os.Stderr
	dbgSeq   int64
	dbgStart = time.Now()
	dbgFlow  fieldSet
)

// fieldSet counts in-flight work per label so a state line can show what is
// running right now.
type fieldSet struct {
	mu sync.Mutex
	m  map[string]int
}

func (f *fieldSet) add(label string, delta int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.m == nil {
		f.m = map[string]int{}
	}
	f.m[label] += delta
	if f.m[label] <= 0 {
		delete(f.m, label)
	}
}

func (f *fieldSet) snapshot() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.m) == 0 {
		return "none"
	}
	keys := make([]string, 0, len(f.m))
	for k := range f.m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%d", k, f.m[k]))
	}
	return strings.Join(parts, ",")
}

// dbg writes one timestamped record. The mutex matters: a multi-kilobyte
// goroutine dump written by one goroutine would otherwise interleave with audit
// lines from another and both become unreadable.
func dbg(format string, args ...any) {
	if !dbgOn {
		return
	}
	msg := fmt.Sprintf(format, args...)
	dbgMu.Lock()
	defer dbgMu.Unlock()
	fmt.Fprintf(dbgOut, "%s %7.1fs %s%s\n",
		time.Now().UTC().Format("15:04:05.000"), time.Since(dbgStart).Seconds(), dbgPrefix, msg)
}

// sessionIDOf pulls the MCP session id from a request context, "-" if none.
// Two clients on two paths get two ids, so every line here is attributable.
func sessionIDOf(ctx context.Context) string {
	if cs := server.ClientSessionFromContext(ctx); cs != nil {
		return cs.SessionID()
	}
	return "-"
}

// workdirOf pulls the injected workdir out of a request context.
func workdirOf(ctx context.Context) string {
	if w, ok := ctx.Value("workdir").(WorkdirContext); ok && w.Dir != "" {
		return w.Dir
	}
	return "-"
}

// reqID renders a JSON-RPC id compactly for start/end correlation.
func reqID(id any) string {
	if b, err := json.Marshal(id); err == nil && len(b) > 0 {
		return string(b)
	}
	return fmt.Sprintf("%v", id)
}

// briefArgs renders tool name + arguments, truncated.
func briefArgs(msg any) string {
	if m, ok := msg.(*mcp.CallToolRequest); ok {
		s := fmt.Sprintf("%s %v", m.Params.Name, m.Params.Arguments)
		if len(s) > 200 {
			s = s[:200] + "..."
		}
		return s
	}
	return ""
}

// deadlineOf renders a context deadline; "none" is itself a useful answer —
// run_terminal_command's cmd is built with exec.Command and no context at all,
// so a hung child can never be cancelled from the request side.
func deadlineOf(ctx context.Context) string {
	d, ok := ctx.Deadline()
	if !ok {
		return "none"
	}
	return fmt.Sprintf("%s (in %s)", d.Format("15:04:05"), time.Until(d).Round(time.Millisecond))
}

// ---------------------------------------------------------------------------
// In-flight table, read by the stall watchdog and by every dump
// ---------------------------------------------------------------------------

type inFlight struct {
	id      string
	method  string
	session string
	workdir string
	began   time.Time
}

var (
	inFlightMu sync.Mutex
	inFlightReqs = map[string]inFlight{}
)

func flightAdd(id, method, session, workdir string) {
	inFlightMu.Lock()
	defer inFlightMu.Unlock()
	inFlightReqs[id+":"+session] = inFlight{id: id, method: method, session: session, workdir: workdir, began: time.Now()}
}

func flightDel(id, session string) {
	inFlightMu.Lock()
	defer inFlightMu.Unlock()
	delete(inFlightReqs, id+":"+session)
}

func flightList() []inFlight {
	inFlightMu.Lock()
	defer inFlightMu.Unlock()
	out := make([]inFlight, 0, len(inFlightReqs))
	for _, v := range inFlightReqs {
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].began.Before(out[j].began) })
	return out
}

// ---------------------------------------------------------------------------
// Process state helpers. These are what distinguish the candidate causes:
// a lock cycle, a blocked write, an unreapable child, a full stderr pipe.
// ---------------------------------------------------------------------------

// procSelf returns the /proc/self/status lines that identify a blocked process.
func procSelf() string {
	b, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return "  (no /proc/self/status: " + err.Error() + ")\n"
	}
	want := map[string]bool{"State": true, "Threads": true, "PPid": true,
		"voluntary_ctxt_switches": true, "nonvoluntary_ctxt_switches": true,
		"SigBlk": true, "SigIgn": true, "SigCgt": true}
	var sb strings.Builder
	for _, line := range strings.Split(string(b), "\n") {
		if want[strings.SplitN(line, ":", 2)[0]] {
			sb.WriteString("  " + strings.TrimSpace(line) + "\n")
		}
	}
	return sb.String()
}

// procChildren lists our children with state and wait channel. A state of 't'
// means ptrace-stopped: such a child can never be reaped, so a cmd.Wait()
// waiting on it never returns and SIGKILL of the parent is the only escape —
// which is exactly the reported "-15 does nothing, -9 works" symptom.
func procChildren() string {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return "  (no /proc)\n"
	}
	mypid := os.Getpid()
	var sb strings.Builder
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid == mypid {
			continue
		}
		st, err := os.ReadFile("/proc/" + e.Name() + "/stat")
		if err != nil {
			continue
		}
		// comm can contain spaces and parens; fields start after the last ')'.
		f := strings.SplitN(string(st), ")", 2)
		if len(f) < 2 {
			continue
		}
		fields := strings.Fields(f[1])
		if len(fields) < 3 {
			continue
		}
		ppid, _ := strconv.Atoi(fields[1])
		if ppid != mypid {
			continue
		}
		pgid, _ := strconv.Atoi(fields[2])
		cl, _ := os.ReadFile("/proc/" + e.Name() + "/cmdline")
		cmd := strings.TrimSpace(strings.ReplaceAll(string(cl), "\x00", " "))
		if len(cmd) > 90 {
			cmd = cmd[:90]
		}
		w, _ := os.ReadFile("/proc/" + e.Name() + "/wchan")
		sb.WriteString(fmt.Sprintf("  child pid=%s pgid=%d state=%s wchan=%s cmd=%q\n",
			e.Name(), pgid, fields[0], strings.TrimSpace(string(w)), cmd))
	}
	if sb.Len() == 0 {
		return "  no children\n"
	}
	return sb.String()
}

// procFDs shows where fd 1 and 2 point and how many pipes/sockets are open. The
// audit log writes to stderr on EVERY tool call: if stderr is a pipe whose
// reader stopped draining, the write blocks at the 64 KiB limit and every
// handler stalls on its next log line — no further log output, which matches
// "no log recorded".
func procFDs() string {
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		return "  (no /proc/self/fd)\n"
	}
	var sb strings.Builder
	for _, e := range entries {
		target, err := os.Readlink("/proc/self/fd/" + e.Name())
		if err != nil {
			continue
		}
		if e.Name() == "0" || e.Name() == "1" || e.Name() == "2" ||
			strings.Contains(target, "pipe:") || strings.Contains(target, "socket:") {
			sb.WriteString(fmt.Sprintf("  fd %s -> %s\n", e.Name(), target))
		}
	}
	return sb.String()
}

// dumpStacks writes every goroutine's stack plus the process state above.
// Stacks are collected into a buffer first so the dump is one write.
func dumpStacks(why string) {
	if !dbgOn {
		return
	}
	buf := make([]byte, 1<<19)
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			buf = buf[:n]
			break
		}
		buf = append(buf, make([]byte, 1<<19)...)
	}
	dbgMu.Lock()
	fmt.Fprintf(dbgOut, "\n%s %7.1fs %sdump %s pid=%d ppid=%d pgid=%d\n",
		time.Now().UTC().Format("15:04:05.000"), time.Since(dbgStart).Seconds(),
		dbgPrefix, why, os.Getpid(), os.Getppid(), dbgPgrp())
	fmt.Fprintf(dbgOut, "  in-flight: %s\n", dbgFlow.snapshot())
	for _, f := range flightList() {
		fmt.Fprintf(dbgOut, "    running: id=%s method=%s session=%s workdir=%s for=%s\n",
			f.id, f.method, f.session, f.workdir, time.Since(f.began).Round(time.Millisecond))
	}
	fmt.Fprint(dbgOut, procSelf())
	fmt.Fprint(dbgOut, procChildren())
	fmt.Fprint(dbgOut, procFDs())
	fmt.Fprintf(dbgOut, "  goroutine stacks (%d bytes):\n%s\n", len(buf), string(buf))
	fmt.Fprintf(dbgOut, "%s---- end dump ----\n", dbgPrefix)
	if f, ok := dbgOut.(*os.File); ok {
		_ = f.Sync()
	}
	dbgMu.Unlock()
}

func dbgDur(name string, def time.Duration) time.Duration {
	if v := os.Getenv(name); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return time.Duration(n) * time.Second
		}
	}
	return def
}

func dbgInt(name string, def int) int {
	if v := os.Getenv(name); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func readCmdline(pid int) string {
	b, _ := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	return strings.TrimSpace(strings.ReplaceAll(strings.ReplaceAll(string(b), "\x00", " "), "\n", " "))
}

// InitDbg turns the tracer on (AIG_DEBUG=1) and installs the signal logger, the
// on-demand stack dump, and the stall watchdog. It is process-wide, so main
// calls it once, before any transport starts.
func InitDbg() {
	if !dbgOn {
		return
	}
	stall := dbgDur("AIG_DEBUG_STALL", 20*time.Second)
	dumpSig := dbgInt("AIG_DEBUG_SIG", int(dbgDumpSignal()))

	dbg("init pid=%d ppid=%d pgid=%d go=%s GOMAXPROCS=%d stall=%s dump-sig=%d",
		os.Getpid(), os.Getppid(), dbgPgrp(), runtime.Version(),
		runtime.GOMAXPROCS(0), stall, dumpSig)
	dbg("init parent pid=%d cmdline=%q", os.Getppid(), readCmdline(os.Getppid()))
	dbg("init fds:\n%s", strings.TrimRight(procFDs(), "\n"))
	dbg("init NOTE: TERM/INT/HUP trigger a graceful shutdown of the streamable listener")
	dbg("init NOTE: (see shutdown.go, grace %s) — they only work if they reach THIS process:", shutdownGrace)
	dbg("init NOTE: if the launcher is a wrapper (go run / a shell / a supervisor), the")
	dbg("init NOTE: signal may land there instead and we keep running. Start the binary")
	dbg("init NOTE: directly and nohup'd so the PID you hold is the one you signal.")

	// Notify on everything. A signal that arrives but is unhandled must be
	// distinguishable from a signal that never reached this process.
	sig := make(chan os.Signal, 32)
	signal.Notify(sig, dbgNotifySignals()...)

	go func() {
		for s := range sig {
			dbg("sig received=%s in-flight=%s", s, dbgFlow.snapshot())
			if dbgSignalNumber(s) == dumpSig {
				dumpStacks("on-demand " + s.String())
				continue
			}
			// Stop signals are also subscribed by waitShutdownSignal, which drives
			// the graceful shutdown; this channel only logs. Shutdown is capped at
			// shutdownGrace, so if the process is STILL ALIVE shortly after, the
			// stacks below say what it is waiting for — usually an exec parked on
			// an inherited pipe, or a client-owned SSE GET that has not closed.
			if dbgIsStopSignal(s) {
				go func(s os.Signal) {
					time.Sleep(300 * time.Millisecond)
					dumpStacks(fmt.Sprintf("%s sent, process STILL ALIVE", s))
				}(s)
			}
		}
	}()

	// Watchdog: dump when the oldest in-flight request passes the threshold,
	// then once per further threshold. This is the record you cannot get any
	// other way once the hang has cost you the ability to poke the process.
	go func() {
		tick := time.NewTicker(stall / 4)
		defer tick.Stop()
		dumps := 0
		for range tick.C {
			list := flightList()
			if len(list) == 0 {
				continue
			}
			oldest := time.Since(list[0].began)
			if oldest < stall {
				continue
			}
			if oldest < time.Duration(dumps+1)*stall && dumps > 0 {
				continue
			}
			dumps++
			dumpStacks(fmt.Sprintf("watchdog: oldest request has run %s (dump #%d)",
				oldest.Round(time.Second), dumps))
		}
	}()
}

// ---------------------------------------------------------------------------
// Instrumentation seams
// ---------------------------------------------------------------------------

// DbgHooks registers request-level tracing. The before/success pair is the
// server-side start and stop of every JSON-RPC request, which is the pair that
// separates "server never answered" from "client never read the answer".
func DbgHooks(hooks *server.Hooks) {
	if !dbgOn || hooks == nil {
		return
	}
	hooks.AddBeforeAny(func(ctx context.Context, id any, method mcp.MCPMethod, message any) {
		s, w := sessionIDOf(ctx), workdirOf(ctx)
		dbgFlow.add("req:"+string(method), 1)
		if method == mcp.MethodToolsCall {
			dbgFlow.add("tool", 1)
		}
		flightAdd(reqID(id), string(method), s, w)
		dbg("req-start id=%s method=%s session=%s workdir=%s args=%q",
			reqID(id), method, s, w, briefArgs(message))
	})
	// Success and error are mutually exclusive per request id; both must clear
	// the flight entry or the watchdog blames a finished request forever.
	hooks.AddOnSuccess(func(ctx context.Context, id any, method mcp.MCPMethod, message any, result any) {
		s := sessionIDOf(ctx)
		flightDel(reqID(id), s)
		dbgFlow.add("req:"+string(method), -1)
		if method == mcp.MethodToolsCall {
			dbgFlow.add("tool", -1)
		}
		dbg("req-end id=%s method=%s session=%s result_bytes=%d in-flight=%s",
			reqID(id), method, s, len(fmt.Sprint(result)), dbgFlow.snapshot())
	})
	hooks.AddOnError(func(ctx context.Context, id any, method mcp.MCPMethod, message any, err error) {
		s := sessionIDOf(ctx)
		flightDel(reqID(id), s)
		dbgFlow.add("req:"+string(method), -1)
		if method == mcp.MethodToolsCall {
			dbgFlow.add("tool", -1)
		}
		dbg("req-error id=%s method=%s session=%s err=%v", reqID(id), method, s, err)
	})
	hooks.AddOnRegisterSession(func(ctx context.Context, sess server.ClientSession) {
		dbg("session-new id=%s workdir=%s in-flight=%s", sess.SessionID(), workdirOf(ctx), dbgFlow.snapshot())
	})
	hooks.AddOnUnregisterSession(func(ctx context.Context, sess server.ClientSession) {
		dbg("session-gone id=%s in-flight=%s", sess.SessionID(), dbgFlow.snapshot())
	})
}

// DbgWrapHandler wraps one tool handler. The handler boundary matters: a handler
// can return while its response is still travelling (SSE path), and from the
// client those two failures look identical.
func DbgWrapHandler(name string, h server.ToolHandlerFunc) server.ToolHandlerFunc {
	if !dbgOn {
		return h
	}
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		s := sessionIDOf(ctx)
		t0 := time.Now()
		dbg("handler-start tool=%s session=%s workdir=%s ctx-deadline=%s",
			name, s, workdirOf(ctx), deadlineOf(ctx))
		res, err := h(ctx, req)
		dbg("handler-end tool=%s session=%s elapsed=%s err=%v",
			name, s, time.Since(t0).Round(time.Millisecond), err)
		return res, err
	}
}

// DbgHTTP wraps the listening handler: request accepted, plus the status code
// and byte count actually written. An http-in with no http-out is stuck in the
// handler chain; no http-in means net/http never dispatched the connection.
func DbgHTTP(h http.Handler) http.Handler {
	if !dbgOn {
		return h
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := &statusRecorder{ResponseWriter: w, status: 200}
		dbgSeq++
		n := dbgSeq
		t0 := time.Now()
		dbg("http-in #%d %s %s proto=%s remote=%s session=%q accept=%q content_length=%d in-flight=%s",
			n, r.Method, r.URL.Path, r.Proto, r.RemoteAddr,
			r.Header.Get("Mcp-Session-Id"), r.Header.Get("Accept"), r.ContentLength, dbgFlow.snapshot())
		defer func() {
			if p := rec.preview(); p != "" {
				dbg("http-out #%d %s %s status=%d bytes=%d elapsed=%s body=%q",
					n, r.Method, r.URL.Path, rec.status, rec.bytes, time.Since(t0).Round(time.Millisecond), p)
				return
			}
			dbg("http-out #%d %s %s status=%d bytes=%d elapsed=%s",
				n, r.Method, r.URL.Path, rec.status, rec.bytes, time.Since(t0).Round(time.Millisecond))
		}()
		h.ServeHTTP(rec, r)
	})
}

// statusRecorder captures the status line and a short body preview. It must
// keep forwarding Flush (the streamable transport streams SSE through it) and
// expose Unwrap, or the tracer would change the behaviour it is observing.
type statusRecorder struct {
	http.ResponseWriter
	status  int
	bytes   int
	head    []byte
	wroteHdr bool
}

func (s *statusRecorder) WriteHeader(code int) {
	if !s.wroteHdr {
		s.status = code
		s.wroteHdr = true
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(p []byte) (int, error) {
	if len(s.head) < 400 {
		s.head = append(s.head, p...)
	}
	n, err := s.ResponseWriter.Write(p)
	s.bytes += n
	return n, err
}

func (s *statusRecorder) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (s *statusRecorder) Unwrap() http.ResponseWriter { return s.ResponseWriter }

func (s *statusRecorder) preview() string {
	if len(s.head) == 0 {
		return ""
	}
	b := s.head
	if len(b) > 300 {
		b = b[:300]
	}
	return strings.TrimSpace(string(b))
}

// DbgExec brackets a blocking cmd.Run(). A long elapsed with no live child
// means we were blocked draining or reaping, not executing.
func DbgExec(tag string, cmd *exec.Cmd, ctx context.Context, run func() error) error {
	if !dbgOn {
		return run()
	}
	dbgFlow.add("exec:"+tag, 1)
	t0 := time.Now()
	dbg("exec-pre %s dir=%q args=%q ctx-deadline=%s", tag, cmd.Dir, strings.Join(cmd.Args, " "), deadlineOf(ctx))
	err := run()
	dbgFlow.add("exec:"+tag, -1)
	pid := -1
	if cmd.Process != nil {
		pid = cmd.Process.Pid
	}
	el := time.Since(t0).Round(time.Millisecond)
	if el > 5*time.Second {
		dbg("exec-post %s elapsed=%s err=%v pid=%d children:\n%s",
			tag, el, err, pid, strings.TrimRight(procChildren(), "\n"))
		return err
	}
	dbg("exec-post %s elapsed=%s err=%v pid=%d", tag, el, err, pid)
	return err
}
