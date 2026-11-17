//go:build !windows

package main

import (
	"os"
	"os/exec"
	"syscall"
)

// setNewGroup puts the child in its own process group so a deadline can signal
// the whole tree. os/exec's own context-cancel only signals the direct child,
// which leaves the grandchildren — the ones holding our capture pipes — alive.
func setNewGroup(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}

// killGroup SIGKILLs the command's process group, falling back to the single
// process if the group is already gone.
func killGroup(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
		_ = cmd.Process.Kill()
	}
}

// dbgPgrp reports the process group, for the startup trace line.
func dbgPgrp() int { return syscall.Getpgrp() }

// dbgDumpSignal is the signal that triggers an on-demand stack dump.
func dbgDumpSignal() int { return int(syscall.SIGQUIT) }

// dbgNotifySignals lists what the tracer subscribes to. Subscribing matters more
// than it looks: os/signal suppresses the runtime's default disposition for every
// signal a channel is registered for, so a signal listed here but never acted on
// keeps the process alive.
func dbgNotifySignals() []os.Signal {
	return []os.Signal{
		syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP, syscall.SIGQUIT,
		syscall.SIGUSR1, syscall.SIGUSR2, syscall.SIGWINCH, syscall.SIGPIPE, syscall.SIGALRM,
	}
}

// dbgShutdownSignals are the signals that stop the server.
func dbgShutdownSignals() []os.Signal {
	return []os.Signal{syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP}
}

// dbgIsStopSignal reports whether s should trigger the graceful shutdown.
func dbgIsStopSignal(s os.Signal) bool {
	ss, ok := s.(syscall.Signal)
	if !ok {
		return false
	}
	return ss == syscall.SIGTERM || ss == syscall.SIGINT || ss == syscall.SIGHUP
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
