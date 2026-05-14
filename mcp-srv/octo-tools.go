package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// OctopusManager handles all Octopus Deploy MCP tool operations.
// Reads OCTO_URL and OCTO_API_KEY from environment variables.
type OctopusManager struct {
	baseURL    string
	apiKey     string
	httpClient *http.Client
}

func NewOctopusManager() *OctopusManager {
	return &OctopusManager{
		baseURL: strings.TrimRight(os.Getenv("OCTO_URL"), "/"),
		apiKey:  os.Getenv("OCTO_API_KEY"),
		// No global Timeout — it covers the entire body read and kills large downloads.
		// Dialer + ResponseHeaderTimeout guard connect/headers only.
		httpClient: &http.Client{
			Transport: &http.Transport{
				DialContext: (&net.Dialer{
					Timeout: 30 * time.Second,
				}).DialContext,
				ResponseHeaderTimeout: 30 * time.Second,
			},
		},
	}
}

// octopusGet performs an authenticated GET against the Octopus REST API.
func (o *OctopusManager) octopusGet(path string) ([]byte, error) {
	req, err := http.NewRequest("GET", o.baseURL+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Octopus-ApiKey", o.apiKey)
	req.Header.Set("Accept", "application/json")

	resp, err := o.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("octopus API error %d: %s", resp.StatusCode, string(body))
	}
	return body, nil
}

// ── octo_download_package ────────────────────────────────────────────────────

func (o *OctopusManager) handleDownloadPackage(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	pattern := argString(req, "pattern")
	if strings.TrimSpace(pattern) == "" {
		return mcp.NewToolResultError("pattern argument is required"), nil
	}

	destDir := argString(req, "dest_dir")
	if destDir == "" {
		destDir = "."
	}
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("cannot create dest_dir: %v", err)), nil
	}

	// filter = substring match; latest=true = one result per package ID (newest)
	limit := argInt(req, "limit", 1)
	searchURL := fmt.Sprintf("/api/packages?filter=%s&latest=true&take=%d", url.QueryEscape(pattern), limit)
	body, err := o.octopusGet(searchURL)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("package search failed: %v", err)), nil
	}

	var result struct {
		Items []struct {
			PackageID     string `json:"PackageId"`
			Version       string `json:"Version"`
			FileExtension string `json:"FileExtension"`
			Links         struct {
				Raw string `json:"Raw"`
			} `json:"Links"`
		} `json:"Items"`
		TotalResults int `json:"TotalResults"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("failed to parse package list: %v", err)), nil
	}

	if len(result.Items) == 0 {
		return mcp.NewToolResultText(fmt.Sprintf("no packages found matching %q", pattern)), nil
	}

	var downloaded []string
	var failures []string

	for _, pkg := range result.Items {
		rawPath := pkg.Links.Raw
		if rawPath == "" {
			failures = append(failures, fmt.Sprintf("%s %s: no raw download link in API response", pkg.PackageID, pkg.Version))
			continue
		}

		dlReq, err := http.NewRequest("GET", o.baseURL+rawPath, nil)
		if err != nil {
			failures = append(failures, fmt.Sprintf("%s %s: build request: %v", pkg.PackageID, pkg.Version, err))
			continue
		}
		dlReq.Header.Set("X-Octopus-ApiKey", o.apiKey)

		resp, err := o.httpClient.Do(dlReq)
		if err != nil {
			failures = append(failures, fmt.Sprintf("%s %s: http: %v", pkg.PackageID, pkg.Version, err))
			continue
		}

		if resp.StatusCode >= 400 {
			errBody, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			failures = append(failures, fmt.Sprintf("%s %s: server %d: %s", pkg.PackageID, pkg.Version, resp.StatusCode, string(errBody)))
			continue
		}

		// Use the extension the API reports (e.g. ".nupkg"), never ".zip"
		ext := pkg.FileExtension
		if ext == "" {
			ext = ".nupkg"
		}
		fileName := filepath.Join(destDir, fmt.Sprintf("%s.%s%s", pkg.PackageID, pkg.Version, ext))
		f, err := os.Create(fileName)
		if err != nil {
			resp.Body.Close()
			failures = append(failures, fmt.Sprintf("%s %s: create file: %v", pkg.PackageID, pkg.Version, err))
			continue
		}

		n, copyErr := io.Copy(f, resp.Body)
		resp.Body.Close()
		f.Close()

		if copyErr != nil {
			os.Remove(fileName)
			failures = append(failures, fmt.Sprintf("%s %s: copy failed after %d bytes: %v", pkg.PackageID, pkg.Version, n, copyErr))
			continue
		}
		if n == 0 {
			os.Remove(fileName)
			failures = append(failures, fmt.Sprintf("%s %s: server returned 0 bytes", pkg.PackageID, pkg.Version))
			continue
		}

		downloaded = append(downloaded, fmt.Sprintf("%s (%d bytes)", fileName, n))
	}

	summary := fmt.Sprintf("Downloaded %d/%d packages matching %q to %q",
		len(downloaded), result.TotalResults, pattern, destDir)
	if len(failures) > 0 {
		summary += fmt.Sprintf("\nFailed (%d):\n  %s", len(failures), strings.Join(failures, "\n  "))
	}
	if len(downloaded) > 0 {
		summary += fmt.Sprintf("\nFiles:\n  %s", strings.Join(downloaded, "\n  "))
	}

	return mcp.NewToolResultText(summary), nil
}

// ── octo_list_packages ───────────────────────────────────────────────────────

func (o *OctopusManager) handleListPackages(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	pattern := argString(req, "pattern")
	latestOnly := argString(req, "latest_only")
	limit := argInt(req, "limit", 1)

	searchURL := fmt.Sprintf("/api/packages?take=%d", limit)
	if pattern != "" {
		searchURL += "&filter=" + url.QueryEscape(pattern)
	}
	if latestOnly != "false" {
		searchURL += "&latest=true"
	}

	body, err := o.octopusGet(searchURL)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("failed to list packages: %v", err)), nil
	}

	var result struct {
		Items []struct {
			PackageID     string `json:"PackageId"`
			Version       string `json:"Version"`
			Published     string `json:"Published"`
			FileExtension string `json:"FileExtension"`
			Size          int64  `json:"PackageSizeBytes"`
		} `json:"Items"`
		TotalResults int `json:"TotalResults"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("failed to parse response: %v", err)), nil
	}

	if len(result.Items) == 0 {
		return mcp.NewToolResultText("no packages found"), nil
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Found %d packages (showing %d):\n\n", result.TotalResults, len(result.Items)))
	for _, p := range result.Items {
		sb.WriteString(fmt.Sprintf("  %-40s %-20s %-10s %s\n", p.PackageID, p.Version, p.FileExtension, p.Published))
	}
	return mcp.NewToolResultText(sb.String()), nil
}

// ── octo_server_stats ────────────────────────────────────────────────────────

func (o *OctopusManager) handleServerStats(_ context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	body, err := o.octopusGet("/api/serverstatus")
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("failed to get server status: %v", err)), nil
	}

	var status map[string]interface{}
	if err := json.Unmarshal(body, &status); err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("failed to parse server status: %v", err)), nil
	}

	pretty, _ := json.MarshalIndent(status, "", "  ")
	return mcp.NewToolResultText(string(pretty)), nil
}

// ── octo_list_deployments ────────────────────────────────────────────────────

func (o *OctopusManager) handleListDeployments(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	project := argString(req, "project")
	env := argString(req, "environment")
	limit := argInt(req, "limit", 1)

	apiURL := fmt.Sprintf("/api/deployments?take=%d", limit)
	if project != "" {
		apiURL += "&projects=" + url.QueryEscape(project)
	}
	if env != "" {
		apiURL += "&environments=" + url.QueryEscape(env)
	}

	body, err := o.octopusGet(apiURL)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("failed to list deployments: %v", err)), nil
	}

	var result struct {
		Items []struct {
			ID            string `json:"Id"`
			ProjectID     string `json:"ProjectId"`
			EnvironmentID string `json:"EnvironmentId"`
			ReleaseID     string `json:"ReleaseId"`
			Created       string `json:"Created"`
			State         string `json:"State"`
		} `json:"Items"`
		TotalResults int `json:"TotalResults"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("failed to parse deployments: %v", err)), nil
	}

	if len(result.Items) == 0 {
		return mcp.NewToolResultText("no deployments found"), nil
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Recent deployments (%d total):\n\n", result.TotalResults))
	for _, d := range result.Items {
		sb.WriteString(fmt.Sprintf("  %-20s  project=%-20s  env=%-15s  %s  [%s]\n",
			d.ID, d.ProjectID, d.EnvironmentID, d.Created, d.State))
	}
	return mcp.NewToolResultText(sb.String()), nil
}

// ── octo_get_release ─────────────────────────────────────────────────────────

func (o *OctopusManager) handleGetRelease(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	project := argString(req, "project")
	if project == "" {
		return mcp.NewToolResultError("project argument is required"), nil
	}

	apiURL := fmt.Sprintf("/api/projects/%s/releases", url.PathEscape(project))
	if ver := argString(req, "version"); ver != "" {
		apiURL = fmt.Sprintf("/api/projects/%s/releases/%s", url.PathEscape(project), url.PathEscape(ver))
	}

	body, err := o.octopusGet(apiURL)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("failed to get release: %v", err)), nil
	}

	var raw interface{}
	if err := json.Unmarshal(body, &raw); err != nil {
		return mcp.NewToolResultError("failed to parse release response"), nil
	}
	pretty, _ := json.MarshalIndent(raw, "", "  ")
	return mcp.NewToolResultText(string(pretty)), nil
}

// ── Registration ─────────────────────────────────────────────────────────────

func registerOctopusTools(s *server.MCPServer, o *OctopusManager) {
	s.AddTool(mcp.NewTool("octo_download_package",
		mcp.WithDescription("Search the Octopus package feed for packages whose ID matches the given pattern and download to disk. Downloads the single latest matching package by default."),
		mcp.WithString("pattern",
			mcp.Required(),
			mcp.Description("Substring pattern to match against package IDs (case-insensitive). E.g. \"MyApp\" matches MyApp.Web, MyApp.Api, etc."),
		),
		mcp.WithString("dest_dir",
			mcp.Description("Local directory to save downloaded packages to (default: current directory)."),
		),
		mcp.WithNumber("limit",
			mcp.Description("Number of matching packages to download (default: 1)."),
		),
	), o.handleDownloadPackage)

	s.AddTool(mcp.NewTool("octo_list_packages",
		mcp.WithDescription("List packages available in the Octopus built-in package feed, optionally filtered by a name pattern."),
		mcp.WithString("pattern",
			mcp.Description("Optional substring filter for package IDs."),
		),
		mcp.WithString("latest_only",
			mcp.Description("Return only the latest version per package ID. true (default) or false to see all versions."),
		),
		mcp.WithNumber("limit",
			mcp.Description("Maximum number of packages to return (default: 1)."),
		),
	), o.handleListPackages)

	s.AddTool(mcp.NewTool("octo_server_stats",
		mcp.WithDescription("Return Octopus Deploy server status information: version, node details, maintenance mode, and system health."),
	), o.handleServerStats)

	s.AddTool(mcp.NewTool("octo_list_deployments",
		mcp.WithDescription("List recent deployments, optionally filtered by project or environment."),
		mcp.WithString("project",
			mcp.Description("Project name or ID to filter by (default: all projects)."),
		),
		mcp.WithString("environment",
			mcp.Description("Environment name or ID to filter by (default: all environments)."),
		),
		mcp.WithNumber("limit",
			mcp.Description("Maximum number of deployments to return (default: 1)."),
		),
	), o.handleListDeployments)

	s.AddTool(mcp.NewTool("octo_get_release",
		mcp.WithDescription("Fetch release details for a project. Returns the latest release if no version is specified."),
		mcp.WithString("project",
			mcp.Required(),
			mcp.Description("Project name or ID (e.g. Projects-1 or \"MyApp\")."),
		),
		mcp.WithString("version",
			mcp.Description("Specific release version to fetch (default: returns all recent releases)."),
		),
	), o.handleGetRelease)
}
