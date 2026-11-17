# Debugging the MCP server: hangs, stalls, and signals

`AIG_DEBUG=1` turns on a request tracer, an automatic stall watchdog, an
on-demand stack dump, and signal logging. Everything goes to **stderr**, so it
lands in the same log file the server already writes — which matters, because
the thing you want to capture is a hang, and during a hang you cannot ask the
server anything.

Implementation: `mcp-srv/debug-trace.go` (tracer + watchdog + dumps),
`mcp-srv/exec-wait.go` (+ `exec_wait_unix.go` / `exec_wait_windows.go`) for the
exec wait cap, `mcp-srv/shutdown.go` for signal handling.

---

## 1. Turn it on

The env is read at process start, so restart the server with it:

```sh
AIG_DEBUG=1 nohup ~/.local/bin/mcp.exe -t streamable -p 8081 ... > /tmp/mcp.log 2>&1 &
echo $! > /tmp/mcp.pid        # keep the PID; this is the process you signal
```

Build with a package path, never a file glob — `go build mcp-srv/*.go` compiles
the files as `command-line-arguments` and both platform files land in one
package (`setNewGroup redeclared`):

```sh
go build -ldflags="-X main.version=v1.0.0-$(git rev-parse --short HEAD) -X main.buildTime=$(date +%Y%m%d_%H%M%S) -extldflags=-static -w -s" \
  --tags "osusergo netgo" -o ~/.local/bin/mcp.exe ./mcp-srv/
GOOS=windows go build ./mcp-srv/   # cross-check
```

| env var | default | meaning |
| --- | --- | --- |
| `AIG_DEBUG=1` | off | enable the tracer, watchdog, signal logger |
| `AIG_DEBUG_STALL=5s` | `20s` | auto-dump once the oldest in-flight request exceeds this. Use `3s` while hunting |
| `AIG_DEBUG_SIG` | `3` (`kill -QUIT`) | signal for an on-demand dump. Windows: `2` (`kill -INT`) |
| `AIG_EXEC_WAIT_DELAY=2s` | `2s` | how long output keeps draining *after* the command exits. `0` = old unbounded behaviour |
| `AIG_EXEC_TIMEOUT=10m` | `10m` | hard ceiling for one command. `0` = unbounded |
| `AIG_SHUTDOWN_GRACE=5s` | `5s` | how long a signal-driven shutdown waits before exiting anyway |

---

## 2. What you see in the log

Every line is tagged `|DBG|` and carries `id`, `session`, `workdir`. One
request produces:

```
|DBG|http-in  #12 POST /mcp/path1 remote=127.0.0.1:44992 session="mcp-session-ab6f..." in-flight=none
|DBG|wm acquire workdir="path1" fullpath="path1" active_workdirs=[path2 path1]
|DBG|req-start id=3 method=tools/call session=mcp-session-ab6f... workdir=path1 args="run_terminal_command map[command:...]"
|DBG|handler-start run_terminal_command session=... workdir=path1
|DBG|exec-pre  run_terminal_command dir="/tmp/base/path1/cmd" args="sh -c ..." ctx-deadline=none
|DBG|exec-post run_terminal_command elapsed=2.001s err=<nil> pid=174950
|DBG|req-end   id=3 method=tools/call session=... result_bytes=512 in-flight=none
|DBG|http-out  #12 POST /mcp/path1 status=200 bytes=560 elapsed=2.004s body="{...}"
```

Other tags: `init` (startup, PID, fds, signal mask), `session-new`, `sig`,
`shutdown`, `wm` / `wm occupied` (workdir contention), `dump`, `---- end dump ----`.
Network decisions are separate: `|NET|allow|...` / `|NET|denied|...`.

**The rule that finds a hang: every `http-in #N` must have an `http-out #N`, and
every `req-start id=N` a `req-end id=N`.** A `http-in` with no matching `http-out`
is provably stuck below that seam — and the last line it *did* print says which
layer: `wm` = workdir manager, `handler-start` = inside the tool handler,
`exec-pre` with no `exec-post` = waiting on a child process.

Handy greps:

```sh
grep '|DBG|http-in' /tmp/mcp.log | grep -v 'in-flight=none'   # overlapping requests
grep 'exec-pre' /tmp/mcp.log                                  # then check for exec-post
grep '|DBG|wm occupied' /tmp/mcp.log                          # workdir contention
grep '|DBG|dump' /tmp/mcp.log                                 # every stall dump
```

---

## 3. What you get when it hangs

### Automatic: the watchdog dump

While any request is older than `AIG_DEBUG_STALL`, a full dump is written every
`STALL` interval — without you touching the process. Real output:

```
10:42:23.422     4.5s |DBG|dump watchdog: oldest request has run 2s (dump #1) pid=195889 ppid=195888 pgid=162571
  in-flight: exec:run_terminal_command=1,req:tools/call=1,tool=1
    running: id=3 method=tools/call session=mcp-session-93f4997f-... workdir=/tmp/mcptest-base/path1 for=2.446s
  State:       S (sleeping)
  PPid:        195888
  Threads:     15
  SigBlk:      fffffffc33ba0000
  SigIgn:      0000000000000000
  SigCgt:      fffffffd7fc1feff
  voluntary_ctxt_switches:     45
  nonvoluntary_ctxt_switches:  0
  no children
  fd 0 -> /dev/null
  fd 1 -> /tmp/stall-demo.log
  fd 8 -> pipe:[586084]        <- capture pipes still open, writer gone
  fd 10 -> pipe:[586085]
  fd 3 -> socket:[586079]
  goroutine stacks (12450 bytes):
  ... every goroutine, full stacks ...
|DBG|---- end dump ----
```

Read it in this order:

1. **`running:` lines** — which request, which client workdir, how long. Two
   `running:` on the *same* workdir points at session contention; on *different*
   workdirs the requests are independent, so a global lock is the suspect.
2. **`children:`** — surviving processes with their command lines. `no children`
   plus an open `pipe:[...]` fd means the wait is on a pipe whose writer already
   exited.
3. **goroutine stacks** — the smoking gun. A goroutine in `os/exec.(*Cmd).Wait`
   or `io.Copy` is the pipe stall; two goroutines in `sync.Mutex.Lock` on the
   same address is a real lock cycle.
4. **`SigBlk`** — if bit 15 (SIGTERM) is set, TERM is *blocked*, which is why
   `kill -15` appears to do nothing.

### Manual: dump on demand

```sh
kill -QUIT $(cat /tmp/mcp.pid)     # one full dump; the server keeps serving
```

### Signals

`kill -15`, `-2`, `-1` are acted on: the log shows `|DBG|sig received=terminated`
then `|DBG|shutdown begin`, the listener closes, and the process exits within
`AIG_SHUTDOWN_GRACE` even if a client still holds an SSE stream open. So
`kill -9` should no longer be needed.

Distinguish the two failure modes:

- **no `sig received=` line at all** → the signal went to a different process
  (a wrapper: `go run`, a shell, a supervisor). Signal the PID from
  `|DBG|init pid=`, not the launcher's.
- **`sig received=` present but process survives** → shutdown is stuck; the
  follow-up dump 300 ms later says what it is waiting for.

---

## 4. The stall this fix addresses

Both exec tools capture output into a `strings.Builder`, so `os/exec` creates an
OS pipe and copies it in a goroutine. `cmd.Run()` waits for the process **and**
for that copy — and the copy only ends when every writer closes the pipe. A
command that leaves a child alive (`foo &`, `nohup`, a daemon, a dev server)
keeps it open, so the tool call blocks for the child's whole lifetime, and a
leaked grandchild blocks forever. `runCapped` caps the pipe wait
(`AIG_EXEC_WAIT_DELAY`) and bounds the whole command
(`AIG_EXEC_TIMEOUT`), killing the command's process group on the deadline.

Measured, same machine, two clients on `/mcp/path1` and `/mcp/path2`:

| case | before | after |
| --- | --- | --- |
| `echo one; sleep 3 & echo two` | ~3 s, both lines | **2.00 s**, both lines intact |
| `sleep 60`, `AIG_EXEC_TIMEOUT=20s` | 60 s / never interruptible | **20.01 s**, `err=signal: killed` |
| plain `echo` | 470 µs | 477 µs (**+0–1 %**, 9 extra allocs) |
| other client while one is blocked | unaffected | unaffected (0.00 s) |

Tradeoff: a command that legitimately streams through a child outliving the
direct process is capped ~`AIG_EXEC_WAIT_DELAY` after the direct child exits.
Raise it, or set `AIG_EXEC_WAIT_DELAY=0 AIG_EXEC_TIMEOUT=0` for the old
behaviour.

---

## 5. Cost of the tracer

Benchmarked (`go test ./mcp-srv/ -run XX -bench . -benchmem`):

| path | `AIG_DEBUG` off | on |
| --- | --- | --- |
| `dbg()` per line | 8–10 ns, 0 allocs | ~380 ns, 6 allocs |
| `DbgHTTP` wrapper | returns handler unchanged, 0 cost | +1 recorder per request |
| in-flight track per request | 111 ns, 1 alloc | same |
| per tool call total | ~0 | ~3 µs on a ~477 µs call (≈0.6 %) |
| log volume | — | ~1.4 KB per request |

`dumpStacks()` is the only heavy call (~8 KB, all goroutines) and it runs only
on a signal or while a request is actually overdue. Leaving `AIG_DEBUG=1` on in
production is fine; set `AIG_DEBUG_STALL=60s` to keep it quiet.

---

## 6. If there is no output at all

- No `|DBG|init pid=` line → the env never reached that process, or you are
  reading the wrong log. A stdio server writes stderr to whoever launched it.
- Process unresponsive and silent:
  `ps -eo pid,ppid,stat,wchan,comm | grep mcp` — `wchan` names the kernel wait:
  `futex` = waiting on a lock, `poll`/`ep_poll` = waiting on I/O.
- Two clients, one path each, and the server refuses one with HTTP 409: that is
  the workdir manager, not a deadlock — see `grep '|DBG|wm occupied'`.
