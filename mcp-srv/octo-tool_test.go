package main

import (
	"context"
	"os"
	"testing"

	mcp "github.com/mark3labs/mcp-go/mcp"
)

// Run with:
//   OCTO_URL=https://your-server OCTO_API_KEY=API-xxx go test -v -tags integration

func newRealOctopus(t *testing.T) *OctopusManager {
	t.Helper()
	octopusURL := os.Getenv("OCTO_URL")
	apiKey := os.Getenv("OCTO_API_KEY")
	if octopusURL == "" || apiKey == "" {
		t.Skip("OCTO_URL and OCTO_API_KEY must be set to run integration tests")
	}
	return &OctopusManager{
		baseURL:    octopusURL,
		apiKey:     apiKey,
		httpClient: NewOctopusManager().httpClient,
	}
}

// ── octo_server_stats ────────────────────────────────────────────────────────

func TestIntegration_ServerStats(t *testing.T) {
	o := newRealOctopus(t)

	res, err := o.handleServerStats(context.Background(), makeReq(nil))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.IsError {
		t.Fatalf("tool returned error: %v", res.Content)
	}
	t.Logf("server stats:\n%s", firstText(res))
}

// ── octo_list_packages ───────────────────────────────────────────────────────

func TestIntegration_ListPackages_NoFilter(t *testing.T) {
	o := newRealOctopus(t)

	res, err := o.handleListPackages(context.Background(), makeReq(nil))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.IsError {
		t.Fatalf("tool returned error: %v", res.Content)
	}
	t.Logf("packages:\n%s", firstText(res))
}

func TestIntegration_ListPackages_WithFilter(t *testing.T) {
	o := newRealOctopus(t)
	pattern := os.Getenv("OCTO_TEST_PACKAGE") // e.g. "MyApp"
	if pattern == "" {
		t.Skip("set OCTO_TEST_PACKAGE to a known package prefix to run this test")
	}

	res, err := o.handleListPackages(context.Background(), makeReq(map[string]any{
		"pattern": pattern,
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.IsError {
		t.Fatalf("tool returned error: %v", res.Content)
	}
	out := firstText(res)
	t.Logf("filtered packages:\n%s", out)
	if out == "no packages found" {
		t.Errorf("expected at least one package matching %q but got none — endpoint or filter may have changed", pattern)
	}
}

// ── octo_download_package ────────────────────────────────────────────────────

func TestIntegration_DownloadPackage(t *testing.T) {
	o := newRealOctopus(t)
	pattern := os.Getenv("OCTO_TEST_PACKAGE")
	if pattern == "" {
		t.Skip("set OCTO_TEST_PACKAGE to a known package prefix to run this test")
	}

	destDir := t.TempDir()
	res, err := o.handleDownloadPackage(context.Background(), makeReq(map[string]any{
		"name_pattern": pattern,
		"dest_dir":     destDir,
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.IsError {
		t.Fatalf("tool returned error: %v", res.Content)
	}

	// Verify at least one file landed on disk
	entries, _ := os.ReadDir(destDir)
	if len(entries) == 0 {
		t.Errorf("no files downloaded to %s — download endpoint may have changed", destDir)
	}
	for _, e := range entries {
		info, _ := e.Info()
		t.Logf("downloaded: %s (%d bytes)", e.Name(), info.Size())
		if info.Size() == 0 {
			t.Errorf("file %s is empty — raw download endpoint may have changed", e.Name())
		}
	}
	t.Logf("dest dir is: %s\n", destDir)
}

// ── octo_list_deployments ────────────────────────────────────────────────────

func TestIntegration_ListDeployments_All(t *testing.T) {
	o := newRealOctopus(t)

	res, err := o.handleListDeployments(context.Background(), makeReq(map[string]any{
		"project": "", "environment": "",
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.IsError {
		t.Fatalf("tool returned error: %v", res.Content)
	}
	t.Logf("deployments:\n%s", firstText(res))
}

func TestIntegration_ListDeployments_ByProject(t *testing.T) {
	o := newRealOctopus(t)
	project := os.Getenv("OCTO_TEST_PROJECT")
	if project == "" {
		t.Skip("set OCTO_TEST_PROJECT to a known project name/ID to run this test")
	}

	res, err := o.handleListDeployments(context.Background(), makeReq(map[string]any{
		"project": project,
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.IsError {
		t.Fatalf("tool returned error: %v", res.Content)
	}
	t.Logf("deployments for %s:\n%s", project, firstText(res))
}

// ── octo_get_release ─────────────────────────────────────────────────────────

func TestIntegration_GetRelease_Latest(t *testing.T) {
	o := newRealOctopus(t)
	project := os.Getenv("OCTO_TEST_PROJECT")
	if project == "" {
		t.Skip("set OCTO_TEST_PROJECT to a known project name/ID to run this test")
	}

	res, err := o.handleGetRelease(context.Background(), makeReq(map[string]any{
		"project": project,
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.IsError {
		t.Fatalf("tool returned error: %v", res.Content)
	}
	t.Logf("latest release:\n%s", firstText(res))
}

// ── helpers ──────────────────────────────────────────────────────────────────

// firstText pulls the first text string out of a tool result's content slice.
func firstText(res *mcp.CallToolResult) string {
	for _, c := range res.Content {
		if c, ok := c.(mcp.TextContent); ok {
			return c.Text
		}
	}
	return ""
}
