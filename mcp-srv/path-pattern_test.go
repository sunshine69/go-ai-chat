package main

// Regression tests for the two patterns run_terminal_command / exec_command use
// for a Go workspace:
//   * execCmdPtnString (and defaultAllowCmd) gate the whole command line,
//   * defaultAllowPath audits every path-shaped token PathPtn scrapes out of it.
// `go get -u ./...`, `go test ./...`, `go vet pkg/...` must pass; chained ".."
// traversal and absolute paths must stay denied.
//
// Run: go test -v -run "Go|Traversal" ./mcp-srv

import (
	"regexp"
	"strings"
	"testing"
)

// commandAllowed mirrors runTerminalCommand's decision path: the whole line must
// match AllowedTerminalCommandPattern (defaultAllowCmd), then every token PathPtn
// scrapes must match AllowedPathPattern (defaultAllowPath) after trimming.
func commandAllowed(cmd string) bool {
	if !regexp.MustCompile(defaultAllowCmd).MatchString(cmd) {
		return false
	}
	allow := regexp.MustCompile(defaultAllowPath)
	for _, tok := range PathPtn.FindAllString(cmd, -1) {
		if !allow.MatchString(strings.TrimSpace(tok)) {
			return false
		}
	}
	return true
}

func TestGoCommandsAllowed(t *testing.T) {
	allowed := []string{
		"go build ./...",
		"go get -u ./...",
		"go test ./...",
		"go vet ./mcp-srv/...",
		"go run a/b/c...",
		"go mod tidy -v",
		"go mod clean",
		"go work .",
		"go workspace",
		"go install gow",
		"go doc -html ./mcp-srv",
		"go build -o bin/app ./mcp-srv",
		"go fmt --diff ./...",
		"go version",
		"go env",
		"go fix ./...",
		"go list ./...",
		"go tool",
		"go pkg build ./...",
		"go bench ./...",
	}
	for _, c := range allowed {
		if !commandAllowed(c) {
			t.Errorf("expected ALLOWED: %s", c)
		}
	}
}

func TestGoWildcardPathsAllowed(t *testing.T) {
	allow := regexp.MustCompile(defaultAllowPath)
	ok := []string{
		"./...", "./.../", "...", "../...", "a/b/c...",
		"./mcp-srv/...", "pkg/config@*",
		".", "./", "x/y", "../x", "../",
		"/tmp/x", "/var/tmp/a/b",
	}
	for _, p := range ok {
		if !allow.MatchString(strings.TrimSpace(p)) {
			t.Errorf("expected path ALLOWED: %s", p)
		}
	}
}

func TestExecGoCommandsAllowed(t *testing.T) {
	ptn := regexp.MustCompile(execCmdPtnString)
	ok := []string{
		"go get -u ./...",
		"go test ./...",
		"go vet ./mcp-srv/...",
		"go build -o bin/app ./mcp-srv",
		"go mod tidy -v",
		"go mod clean",
		"go work .",
		"go workspace",
		"go pkg build ./...",
		"go install gow",
		"go bench ./...",
	}
	for _, c := range ok {
		if !ptn.MatchString(c) {
			t.Errorf("expected exec_command ALLOWED: %s", c)
		}
	}
	denied := []string{
		"go get -u ./...; rm -rf /",
		"go build ./... | sh",
		"go test ./... && curl http://evil",
		"rm -rf ./...",
	}
	for _, c := range denied {
		if ptn.MatchString(c) {
			t.Errorf("expected exec_command DENIED: %s", c)
		}
	}
}

func TestTraversalStaysDenied(t *testing.T) {
	// A single "../" step is allowed by design; only chained ".." hops and absolute
	// paths outside /tmp are denied.
	allow := regexp.MustCompile(defaultAllowPath)
	denied := []string{
		"..", "../../etc/passwd", "/..", "../..", "./../x",
		"/etc/passwd", "/", "/etc",
	}
	for _, p := range denied {
		if allow.MatchString(strings.TrimSpace(p)) {
			t.Errorf("expected path DENIED: %s", p)
		}
	}
	// A traversal token inside an otherwise-permitted go command denies the line.
	for _, c := range []string{"go clean ../..", "go vet ../../etc", "go test /etc"} {
		if commandAllowed(c) {
			t.Errorf("expected command DENIED: %s", c)
		}
	}
}
