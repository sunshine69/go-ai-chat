package main

import (
	"os"
	"os/signal"
	"time"
)

// shutdownGrace bounds how long a signal-driven shutdown waits for in-flight
// work. Keep it short: http.Server.Shutdown waits for every connection to
// finish, and a streamable-HTTP client holds a long-lived GET open on purpose,
// so "wait for the connections" means "wait for the clients to leave". A server
// that does that cannot be stopped with TERM while anyone is still connected,
// which is exactly the "kill -15 does nothing, I have to use -9" report.
//
// Override with AIG_SHUTDOWN_GRACE (Go duration or plain seconds).
var shutdownGrace = envDurationOr("AIG_SHUTDOWN_GRACE", 5*time.Second)

// waitShutdownSignal blocks until TERM/INT/HUP arrives and returns it.
//
// Registering a channel here is what makes TERM work at all: os/signal drops the
// runtime's default "exit now" behaviour for any signal a channel is registered
// for, so a binary that only subscribes for logging (see debug-trace.go) will
// log the SIGTERM and then keep running forever unless something also acts on
// it. That trap is easy to hit — and it looks precisely like a wedged server.
func waitShutdownSignal() os.Signal {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, dbgShutdownSignals()...)
	s := <-ch
	signal.Stop(ch)
	return s
}
