# apply.py - strict, deny-by-default exec_command policy.
# Run from the repo root with working_dir set to mcp-srv.

import os
import re

HERE = os.path.dirname(os.path.abspath(__file__))
ROOT = os.path.dirname(HERE)  # project root
MCP = os.path.join(HERE, "mcp-srv")

MAIN = os.path.join(MCP, "main.go")
BASE = os.path.join(MCP, "base-tool.go")

# ---------------------------------------------------------------------------
# main.go edits
# ---------------------------------------------------------------------------

s = open(MAIN, encoding="utf-8").read()

# 1) var block: add execCommandPattern + policy maps after PathPtn.
marker = "PathPtn             *regexp.Regexp\n"
assert marker in s, "PathPtn var line not found"
addition = marker + (
    "\texecCommandPattern  *regexp.Regexp\n"
    "\t// hardBlockArg0: base program names that are NEVER permitted as the executable\n"
    "\t// via exec_command, even if allowlisted. These reintroduce a shell or cross a\n"
    "\t// privilege/execution boundary, defeating the no-shell guarantee.\n"
    "\thardBlockArg0 = map[string]bool{\n"
    "\t\t\"sh\": true, \"bash\": true, \"zsh\": true, \"dash\": true, \"ksh\": true,\n"
    "\t\t\"fish\": true, \"csh\": true, \"tcsh\": true,\n"
    "\t\t\"cmd\": true, \"cmd.exe\": true, \"powershell\": true, \"pwsh\": true, \"wsl\": true,\n"
    "\t\t\"env\": true, \"xargs\": true, \"nohup\": true, \"setsid\": true, \"run\": true,\n"
    "\t\t\"sudo\": true, \"su\": true, \"doas\": true, \"pkexec\": true,\n"
    "\t}\n"
    "\t// execAllowed: deny-by-default allowlist of bare program names. exec_command\n"
    "\t// requires the resolved program (after LookPath on PATH) to be present here.\n"
    "\t// Policy knob: edit this set. Keep it tight to the dev tooling this server exists\n"
    "\t// to run.\n"
    "\texecAllowed = map[string]bool{\n"
    "\t\t\"go\": true, \"gofmt\": true, \"vet\": true,\n"
    "\t\t\"cargo\": true, \"rustc\": true, \"rustfmt\": true,\n"
    "\t\t\"python3\": true, \"node\": true, \"php\": true, \"ruby\": true, \"perl\": true,\n"
    "\t\t\"git\": true, \"make\": true, \"cmake\": true, \"ninja\": true,\n"
    "\t\t\"jq\": true, \"yq\": true, \"grep\": true, \"curl\": true, \"wget\": true,\n"
    "\t}\n"
)
s = s.replace(marker, addition, 1)

# 2) init close: after the platform switch closes, before init() closes.
i = s.rfind("PathPtn = regexp.MustCompile(")
assert i != -1, "unix PathPtn assignment not found"
tail = s.index(")\n\t}\n}\n", i) + len(")\n\t}\n}\n")
old_tail = s[i:tail]
assert old_tail.endswith("}\n"), "unexpected init close"

pattern_go = (
    "\texecCommandPattern = regexp.MustCompile(`(?i)^` +\n"
    "\t\t`(?:go(?:[ -](?:build|vet|fmt|run|test|install|tidy))?[ -](?:-[^ ]+(?: +[^;|&<>\"']*)*)?|` +\n"
    "\t\t`rustc(?: +-[^ ]+(?: +[^;|&<>\"']*)*)?|` +\n"
    "\t\t`cargo(?: +(?:build|run|test|fmt|check|clippy)(?: +[^;|&<>\"']*)*)?|` +\n"
    "\t\t`git(?: +(?:[a-zA-Z][^;|&<>\"']*)+)?|` +\n"
    "\t\t`python3(?: +[^;|&<>\"']*)*|` +\n"
    "\t\t`node(?: +[^;|&<>\"']*)*|` +\n"
    "\t\t`php(?: +[^;|&<>\"']*)*|` +\n"
    "\t\t`ruby(?: +[^;|&<>\"']*)*|` +\n"
    "\t\t`perl(?: +[^;|&<>\"']*)*|` +\n"
    "\t\t`make(?: +[^;|&<>\"']*)*|` +\n"
    "\t\t`cmake(?: +[^;|&<>\"']*)*|` +\n"
    "\t\t`ninja(?: +[^;|&<>\"']*)*|` +\n"
    "\t\t`jq(?: +[^;|&<>\"']*)*|` +\n"
    "\t\t`yq(?: +[^;|&<>\"']*)*|` +\n"
    "\t\t`grep(?: +[^;|&<>\"']*)*|` +\n"
    "\t\t`curl(?: +[^;|&<>\"']*)*|` +\n"
    "\t\t`wget(?: +[^;|&<>\"']*)*$`)\n"
)
new_tail = old_tail[:-2] + pattern_go + "}\n"
s = s.replace(old_tail, new_tail, 1)

open(MAIN, "w", encoding="utf-8").write(s)
print("main.go patched")

# ---------------------------------------------------------------------------
# base-tool.go edits - replace execCommand + add hasShellMeta helper.
# ---------------------------------------------------------------------------

b = open(BASE, encoding="utf-8").read()

start = b.index("func (t *BaseToolManager) execCommand(")
end = b.index("func CheckForbiddenString")
old_fn = b[start:end]
assert "shlex.Split(command)" in old_fn, "unexpected old execCommand"

new_fn = (
"func (t *BaseToolManager) execCommand(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {\n"
"\targs := request.GetArguments()\n"
"\tcommand := \"\"\n"
"\tif c, ok := args[\"command\"]; ok {\n"
"\t\tcommand = c.(string)\n"
"\t}\n"
"\tif strings.TrimSpace(command) == \"\" {\n"
"\t\treturn mcp.NewToolResultText(\"[ERROR] empty command\"), fmt.Errorf(\"[ERROR] empty command\")\n"
"\t}\n"
"\n"
"\t// ---- STRICT EXEC POLICY (deny-by-default, no shell, allowlisted program) ----\n"
"\t// 1) Tokenize WITHOUT a shell. shlex.Split performs NO expansion: ';' '|' '&'<br>\n"
"\t//    '<' '>' '$' backtick all pass through as literal tokens and are never<br>\n"
"\t//    interpreted, so operators can never cause command chaining.<br>\n"
"\targv, err := shlex.Split(command)\n"
"\tif err != nil || len(argv) == 0 {\n"
"\t\treturn mcp.NewToolResultText(\"[ERROR]\"), fmt.Errorf(\"[ERROR] failed to parse command: %w\", err)\n"
"\t}\n"
"\n"
"\t// 2) Highest gate: the FULL command string must match the allowlist pattern.<br>\n"
"\t//    Any program not enumerated here is denied before any lookup happens.<br>\n"
"\tif !execCommandPattern.MatchString(command) {\n"
"\t\treturn mcp.NewToolResultText(\"[ERROR]\"), fmt.Errorf(\"[ERROR] denied access for command '%s': program not permitted by exec_command policy\", command)\n"
"\t}\n"
"\n"
"\t// 3) Reject any token carrying shell metacharacters. There is no shell, so these<br>\n"
"\t//    are inert, but rejecting them keeps the contract strict and blocks $(), ``, ${}.<br>\n"
"\tfor _, a := range argv {\n"
"\t\tif hasShellMeta(a) {\n"
"\t\t\treturn mcp.NewToolResultText(\"[ERROR]\"), fmt.Errorf(\"[ERROR] denied access for command '%s': shell metacharacter in argument\", command)\n"
"\t\t}\n"
"\t}\n"
"\n"
"\t// 4) Resolve argv[0]: no path component, on PATH, not a shell/indirection tool,<br>\n"
"\t//    and explicitly on the program allowlist.<br>\n"
"\tprogram := argv[0]\n"
"\tif program == \"\" {\n"
"\t\treturn mcp.NewToolResultText(\"[ERROR]\"), fmt.Errorf(\"[ERROR] empty program\")\n"
"\t}\n"
"\tif strings.ContainsAny(program, `/\\\\`) {\n"
"\t\treturn mcp.NewToolResultText(\"[ERROR]\"), fmt.Errorf(\"[ERROR] denied access for command '%s': only bare program names are allowed\", command)\n"
"\t}\n"
"\tif hardBlockArg0[program] {\n"
"\t\treturn mcp.NewToolResultText(\"[ERROR]\"), fmt.Errorf(\"[ERROR] denied access for command '%s': program '%s' is not permitted by exec_command policy\", command, program)\n"
"\t}\n"
"\tabs, err := exec.LookPath(program)\n"
"\tif err != nil {\n"
"\t\treturn mcp.NewToolResultText(\"[ERROR]\"), fmt.Errorf(\"[ERROR] program '%s' not found on PATH: %w\", program, err)\n"
"\t}\n"
"\tif !execAllowed[program] {\n"
"\t\treturn mcp.NewToolResultText(\"[ERROR]\"), fmt.Errorf(\"[ERROR] denied access for command '%s': program '%s' is not permitted by exec_command policy\", command, program)\n"
"\t}\n"
"\n"
"\t// 5) Path-check every path-like argument (and the working dir).<br>\n"
"\tworkingDir := \"./\"\n"
"\tif wd, ok := args[\"working_dir\"]; ok {\n"
"\t\tif w := strings.TrimSpace(fmt.Sprintf(\"%v\", wd)); w != \"\" {\n"
"\t\t\tif res, perr := t.checkPath(w); perr != nil {\n"
"\t\t\t\treturn res, perr\n"
"\t\t\t}\n"
"\t\t\tworkingDir = filepath.Clean(w)\n"
"\t\t}\n"
"\t}\n"
"\tfor _, a := range argv[1:] {\n"
"\t\tif strings.HasPrefix(a, \".\") || strings.HasPrefix(a, \"/\") {\n"
"\t\t\tif res, perr := t.checkPath(a); perr != nil {\n"
"\t\t\t\treturn res, perr\n"
"\t\t\t}\n"
"\t\t}\n"
"\t}\n"
"\n"
"\t// 6) Execute the resolved absolute path with a context/timeout. No shell.<br>\n"
"\texecCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)\n"
"\tdefer cancel()\n"
"\trunCmd := exec.CommandContext(execCtx, abs, argv[1:]...)\n"
"\trunCmd.Dir = workingDir\n"
"\n"
"\tvar stdout, stderr strings.Builder\n"
"\trunCmd.Stdout = &stdout\n"
"\trunCmd.Stderr = &stderr\n"
"\n"
"\trunErr := runCmd.Run()\n"
"\n"
"\tvar sb strings.Builder\n"
"\tsb.WriteString(fmt.Sprintf(\"$ %s\\n\", command))\n"
"\tif stdout.Len() > 0 {\n"
"\t\tsb.WriteString(\"\\n--- stdout ---\\n\")\n"
"\t\tsb.WriteString(stdout.String())\n"
"\t}\n"
"\tif stderr.Len() > 0 {\n"
"\t\tsb.WriteString(\"\\n--- stderr ---\\n\")\n"
"\t\tsb.WriteString(stderr.String())\n"
"\t}\n"
"\tif runErr != nil {\n"
"\t\tsb.WriteString(\"\\n--- exit error ---\\n%s\\n\", runErr.Error())\n"
"\t}\n"
"\tif stdout.Len() == 0 && stderr.Len() == 0 && runErr == nil {\n"
"\t\tsb.WriteString(\"(command completed with no output)\\n\")\n"
"\t}\n"
"\tif sb.Len() >= MAX_OUPUT_SIZE {\n"
"\t\tif tempfile, err := os.CreateTemp(\"\", \"mcp\"); err == nil {\n"
"\t\t\tif _, err := tempfile.Write([]byte(sb.String())); err != nil {\n"
"\t\t\t\treturn nil, err\n"
"\t\t\t}\n"
"\t\t\ttempfile.Sync()\n"
"\t\t\treturn mcp.NewToolResultText(\"Command output is saved to a file \" + tempfile.Name() + \"\\nYou MUST use text tools to extract information. DON'T read full content. REMEMBER to remove it after use.\"), nil\n"
"\t\t} else {\n"
"\t\t\treturn nil, fmt.Errorf(\"[ERROR] can not create tempfile\")\n"
"\t\t}\n"
"\t} else {\n"
"\t\treturn mcp.NewToolResultText(sb.String()), nil\n"
"\t}\n"
"}\n"
"\n"
"\n"
"// hasShellMeta reports whether s contains any byte a shell would interpret for<br>\n"
"// redirection, chaining, or command substitution. exec_command runs programs<br>\n"
"// with NO shell (so such bytes are inert), but rejecting them keeps the contract<br>\n"
"// strict and documents intent.<br>\n"
"func hasShellMeta(s string) bool {\n"
"\tfor _, r := range s {\n"
"\t\tif r == ';' || r == '|' || r == '&' || r == '<' || r == '>' ||\n"
"\t\t\tr == '$' || r == '`' || r == '\\n' || r == '\\r' ||\n"
"\t\t\tr == '*' || r == '?' || r == '~' {\n"
"\t\t\treturn true\n"
"\t\t}\n"
"\t}\n"
"\treturn false\n"
"}\n"
)

b = b[:start] + new_fn + b[end:]
open(BASE, "w", encoding="utf-8").write(b)
print("base-tool.go patched")
print("done")
