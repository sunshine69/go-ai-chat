//go:build windows

package main

import (
	"os"
	"os/exec"
	"syscall"
)

// ---------------------------------------------------------------------------
// Windows side of runCapped and of the tracer's signal handling.
//
// The stall fix (cmd.WaitDelay) is platform-neutral, so Windows gets it too.
// Only the process-group kill degrades: SysProcAttr on Windows has no Setpgid,
// and the equivalent needs a job object, so we kill just the command's process.
//
// Signal names are deliberately spelled as numbers here: syscall's named signal
// constants are not all exported on Windows, and os/signal there can only
// deliver Ctrl-C anyway. SIGINT is 2, SIGTERM is 15.
// ---------------------------------------------------------------------------

const (
	windowsSIGINT  = 2
	windowsSIGTERM = 15
)

// setNewGroup is a no-op on Windows.
func setNewGroup(cmd *exec.Cmd) {}

// killGroup falls back to killing just the command's process.
func killGroup(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}

// dbgPgrp: process groups are a Unix concept.
func dbgPgrp() int { return -1 }

// dbgDumpSignal: SIGQUIT's built-in stack dump is Unix-only, so on Windows the
// on-demand dump is triggered with Ctrl-C.
func dbgDumpSignal() int { return windowsSIGINT }

// dbgNotifySignals: os/signal on Windows only ever delivers Ctrl-C.
func dbgNotifySignals() []os.Signal {
	return []os.Signal{syscall.Signal(windowsSIGINT)}
}

// dbgShutdownSignals are the signals that stop the server.
func dbgShutdownSignals() []os.Signal { return dbgNotifySignals() }

// dbgIsStopSignal reports whether s should trigger the graceful shutdown.
func dbgIsStopSignal(s os.Signal) bool {
	n := dbgSignalNumber(s)
	return n == windowsSIGINT || n == windowsSIGTERM
}

// dbgSignalNumber reports s's signal number, or -1 if it is not a plain
// syscall signal. Used to compare against the configurable dump signal.
func dbgSignalNumber(s os.Signal) int {
	ss, ok := s.(syscall.Signal)
	if !ok {
		return -1
	}
	return int(ss)
}
