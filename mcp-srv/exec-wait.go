package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// runCapped: run() that cannot block forever on an exited command.
//
// Both exec tools wire the command's stdout/stderr into strings.Builder, which
// makes os/exec create an OS pipe and copy it in a goroutine. cmd.Run() then
// waits for BOTH the process to exit AND the copy to finish — and the copy only
// finishes when every descriptor on the write end is closed. A command that
// leaves a child alive (the shell's `foo &`, nohup, a daemon, a dev server, a
// compiler that forks) keeps that write end open, so Run() returns only when the
// last such child exits. That is the reproduced stall: `nohup sleep 8 ... &`
// inside a tool call holds the handler for 8 s; a `sleep 120 &` holds it for two
// minutes; a leaked grandchild holds it indefinitely.
//
// It is also the "kill -15 doesn't work, need -9" symptom, from the same cause:
// while the handler is parked in Run(), the request goroutine is stuck, and if
// TERM/HUP is delivered to a launcher (go run, a shell wrapper) rather than to
// the server process itself, the server keeps running and the client sees a
// permanently hung session.
//
// The fix, in two parts, both of which keep the normal case byte-identical:
//
//   - Setpgid: the command gets its own process group, so a deadline can kill
//     the whole tree instead of just the shell.
//   - WaitDelay: once the command itself has exited, os/exec stops waiting for
//     the inherited pipe after this grace period and closes it. Output already
//     written is still delivered (os/exec drains what is buffered), so
//     `sleep 8 & echo started` returns in ~the grace period with "started" in
//     hand rather than after 8 seconds.
//
// ErrWaitDelay is reported as success when the command exited 0 — the wait that
// timed out belonged to someone else's process, not to the command the client
// asked us to run.
//
// Set AIG_EXEC_WAIT_DELAY=0 to restore the old, unbounded behaviour.
// ---------------------------------------------------------------------------

// execWaitDelay is how long we keep draining the command's output after the
// command itself has exited. Long enough for a normal pipeline to flush, short
// enough that a stray background child cannot wedge the session.
var execWaitDelay = envDurationOr("AIG_EXEC_WAIT_DELAY", 2*time.Second)

// execMaxRun bounds a single tool-driven command. The two call sites used to
// differ (exec_command had 2 minutes, run_terminal_command had nothing at all);
// they now share one ceiling so a forgotten `tail -f` cannot hold a session open
// forever. Override with AIG_EXEC_TIMEOUT (e.g. 10m), or 0 to disable.
var execMaxRun = envDurationOr("AIG_EXEC_TIMEOUT", 10*time.Minute)

// envDurationOr reads a duration from the environment, accepting either a Go
// duration ("2s", "10m") or a bare number of seconds ("2", "0.5") since the
// other knobs in this server take plain seconds. "0"/"off" disables the limit.
func envDurationOr(name string, def time.Duration) time.Duration {
	v := strings.TrimSpace(os.Getenv(name))
	if v == "" {
		return def
	}
	if strings.EqualFold(v, "off") || strings.EqualFold(v, "none") ||
		strings.EqualFold(v, "disable") || strings.EqualFold(v, "disabled") {
		return 0
	}
	if d, err := time.ParseDuration(v); err == nil && d >= 0 {
		return d
	}
	if secs, err := strconv.ParseFloat(v, 64); err == nil && secs >= 0 {
		return time.Duration(secs * float64(time.Second))
	}
	return def
}

// runCapped runs cmd, bounding both the wait for the process and the wait for
// its inherited pipes. ctx is the caller's context; execMaxRun is applied on top
// of it. It returns the same error os/exec would, minus ErrWaitDelay noise, plus
// a marker when we had to kill the process group.
func runCapped(ctx context.Context, cmd *exec.Cmd) error {
	if runtime.GOOS != "windows" {
		setNewGroup(cmd)
	}
	if execWaitDelay > 0 {
		cmd.WaitDelay = execWaitDelay
	}

	// Bound the wait even when the caller passed a background context, which is
	// what the run_terminal_command handler does.
	if execMaxRun > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, execMaxRun)
		defer cancel()
	}

	if execWaitDelay == 0 && execMaxRun == 0 {
		// Old behaviour: unbounded, no context, no group.
		return cmd.Run()
	}

	// Drive the process through the context ourselves so cancelling (or the
	// deadline firing) kills the whole group, then collect the pipes.
	if err := cmd.Start(); err != nil {
		return err
	}

	waited := make(chan error, 1)
	go func() { waited <- cmd.Wait() }()

	var err error
	select {
	case err = <-waited:
	case <-ctx.Done():
		killGroup(cmd)
		// os/exec's Wait can still be parked on the pipes; give it the grace
		// period, then take whatever it managed to collect.
		grace := execWaitDelay
		if grace <= 0 {
			grace = 5 * time.Second
		}
		select {
		case err = <-waited:
		case <-time.After(grace):
			err = errors.Join(ctx.Err(), errors.New("command still running after kill; giving up on it"))
		}
	}

	// A command that finished cleanly but left someone else holding the pipes is
	// a success, not an error.
	if err != nil && errors.Is(err, exec.ErrWaitDelay) {
		if ps := cmd.ProcessState; ps != nil && ps.Success() {
			err = nil
		}
	}
	return err
}

// setNewGroup / killGroup / dbgPgrp are platform-specific: see
// exec_wait_unix.go and exec_wait_windows.go.
