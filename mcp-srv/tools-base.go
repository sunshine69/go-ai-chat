package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	html2md "github.com/JohannesKaufmann/html-to-markdown"
	mcp "github.com/mark3labs/mcp-go/mcp"
)

func readFileContent(targetPath string) (string, error) {
	cleanPath := filepath.Clean(targetPath)
	content, err := os.ReadFile(cleanPath)
	if err != nil {
		return "", fmt.Errorf("failed to read file: %w", err)
	}
	return string(content), nil
}

func listDirectory(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := request.GetArguments()
	path := ""
	if p, ok := args["path"]; ok {
		path = fmt.Sprintf("%v", p)
	}
	cleanPath := filepath.Clean(path)
	entries, err := os.ReadDir(cleanPath)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Directory listing for: %s\n", cleanPath))
	sb.WriteString(strings.Repeat("-", 40) + "\n")
	for _, entry := range entries {
		icon := "📄 "
		if entry.IsDir() {
			icon = "📁 "
		}
		sb.WriteString(fmt.Sprintf("%s%s\n", icon, entry.Name()))
	}
	return mcp.NewToolResultText(sb.String()), nil
}

func createNewFile(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := request.GetArguments()
	path := ""
	if p, ok := args["path"]; ok {
		path = fmt.Sprintf("%v", p)
	}
	content := ""
	if c, ok := args["content"]; ok {
		content = fmt.Sprintf("%v", c)
	}
	overwrite := false
	if o, ok := args["overwrite"]; ok {
		if bv, ok2 := o.(bool); ok2 {
			overwrite = bv
		}
	}

	cleanPath := filepath.Clean(path)
	if err := os.MkdirAll(filepath.Dir(cleanPath), 0755); err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	if !overwrite {
		if _, err := os.Stat(cleanPath); err == nil {
			return mcp.NewToolResultError(fmt.Sprintf("file already exists: %s (set overwrite=true to replace it)", cleanPath)), nil
		}
	}

	flags := os.O_WRONLY | os.O_CREATE | os.O_TRUNC
	f, err := os.OpenFile(cleanPath, flags, 0644)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	defer f.Close()

	if _, err := f.WriteString(content); err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	return mcp.NewToolResultText(fmt.Sprintf("File created successfully: %s (%d bytes)", cleanPath, len(content))), nil
}

func runTerminalCommand(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := request.GetArguments()
	command := ""
	if c, ok := args["command"]; ok {
		command = fmt.Sprintf("%v", c)
	}
	workingDir := ""
	if wd, ok := args["working_dir"]; ok {
		workingDir = fmt.Sprintf("%v", wd)
	}
	confirmed := false
	if cf, ok := args["confirmed"]; ok {
		if bv, ok2 := cf.(bool); ok2 {
			confirmed = bv
		}
	}

	if !confirmed {
		return mcp.NewToolResultText(fmt.Sprintf(
			"⚠️  Confirmation required\n\nThe following command has NOT been executed yet:\n\n  %s\n\nTo run it, call run_terminal_command again with confirmed=true.",
			command,
		)), nil
	}

	cmd := exec.Command("sh", "-c", command)
	if workingDir != "" {
		cmd.Dir = filepath.Clean(workingDir)
	}

	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	runErr := cmd.Run()

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("$ %s\n", command))
	if stdout.Len() > 0 {
		sb.WriteString("\n--- stdout ---\n")
		sb.WriteString(stdout.String())
	}
	if stderr.Len() > 0 {
		sb.WriteString("\n--- stderr ---\n")
		sb.WriteString(stderr.String())
	}
	if runErr != nil {
		sb.WriteString(fmt.Sprintf("\n--- exit error ---\n%s\n", runErr.Error()))
	}
	if stdout.Len() == 0 && stderr.Len() == 0 && runErr == nil {
		sb.WriteString("(command completed with no output)\n")
	}

	return mcp.NewToolResultText(sb.String()), nil
}

func fileGlobSearch(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := request.GetArguments()
	root := ""
	if r, ok := args["root"]; ok {
		root = fmt.Sprintf("%v", r)
	}
	pattern := ""
	if p, ok := args["pattern"]; ok {
		pattern = fmt.Sprintf("%v", p)
	}
	maxResults := 100
	if mr, ok := args["max_results"]; ok {
		switch v := mr.(type) {
		case float64:
			maxResults = int(v)
		case int:
			maxResults = v
		default:
			maxResults = 100
		}
	}

	cleanRoot := filepath.Clean(root)
	namePattern := pattern
	recursive := false
	if strings.HasPrefix(pattern, "**/") {
		recursive = true
		namePattern = strings.TrimPrefix(pattern, "**/")
	}

	var matches []string
	err := filepath.WalkDir(cleanRoot, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		matched := false
		var matchErr error
		if recursive {
			matched, matchErr = filepath.Match(namePattern, d.Name())
		} else {
			rel, _ := filepath.Rel(cleanRoot, path)
			matched, matchErr = filepath.Match(pattern, rel)
		}
		if matchErr != nil {
			return fmt.Errorf("invalid glob pattern: %w", matchErr)
		}
		if matched {
			matches = append(matches, path)
			if len(matches) >= maxResults {
				return io.EOF
			}
		}
		return nil
	})

	if err != nil && err != io.EOF {
		return mcp.NewToolResultError(err.Error()), nil
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Found %d file(s) matching '%s' under %s", len(matches), pattern, cleanRoot))
	if len(matches) == maxResults {
		sb.WriteString(fmt.Sprintf(" (result capped at %d)", maxResults))
	}
	sb.WriteString(":\n")
	for _, m := range matches {
		sb.WriteString("  " + m + "\n")
	}

	return mcp.NewToolResultText(sb.String()), nil
}

func findReplaceInFile(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := request.GetArguments()
	path := ""
	if p, ok := args["path"]; ok {
		path = fmt.Sprintf("%v", p)
	}
	find := ""
	if f, ok := args["find"]; ok {
		find = fmt.Sprintf("%v", f)
	}
	replace := ""
	if r, ok := args["replace"]; ok {
		replace = fmt.Sprintf("%v", r)
	}
	useRegex := false
	if u, ok := args["use_regex"]; ok {
		if bv, ok2 := u.(bool); ok2 {
			useRegex = bv
		}
	}
	replaceAll := false
	if ra, ok := args["replace_all"]; ok {
		if bv, ok2 := ra.(bool); ok2 {
			replaceAll = bv
		}
	}

	cleanPath := filepath.Clean(path)
	data, err := os.ReadFile(cleanPath)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	original := string(data)

	var updated string
	var count int

	if useRegex {
		re, compileErr := regexp.Compile(find)
		if compileErr != nil {
			return mcp.NewToolResultError(fmt.Sprintf("invalid regex pattern: %w", compileErr)), nil
		}
		if replaceAll {
			count = len(re.FindAllString(original, -1))
			updated = re.ReplaceAllString(original, replace)
		} else {
			loc := re.FindStringIndex(original)
			if loc != nil {
				count = 1
				updated = original[:loc[0]] + re.ReplaceAllString(original[loc[0]:loc[1]], replace) + original[loc[1]:]
			} else {
				updated = original
			}
		}
	} else {
		if replaceAll {
			count = strings.Count(original, find)
			updated = strings.ReplaceAll(original, find, replace)
		} else {
			idx := strings.Index(original, find)
			if idx >= 0 {
				count = 1
				updated = original[:idx] + replace + original[idx+len(find):]
			} else {
				updated = original
			}
		}
	}

	if count == 0 {
		return mcp.NewToolResultText(fmt.Sprintf("Pattern not found in %s — file unchanged.", cleanPath)), nil
	}

	if err := os.WriteFile(cleanPath, []byte(updated), 0644); err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	noun := "occurrence"
	if count != 1 {
		noun = "occurrences"
	}
	return mcp.NewToolResultText(fmt.Sprintf("Replaced %d %s of %q in %s.", count, noun, find, cleanPath)), nil
}

func fetchAndConvert(url string) (string, error) {
	client := &http.Client{}
	httpReq, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return "", fmt.Errorf("invalid URL: %w", err)
	}
	httpReq.Header.Set("User-Agent", "MCP-Stdio-Server/1.0")

	resp, err := client.Do(httpReq)
	if err != nil {
		return "", fmt.Errorf("fetch failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("HTTP error: %d %s\nResponse body: %s", resp.StatusCode, resp.Status, string(body))
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("read body failed: %w", err)
	}

	converter := html2md.NewConverter("", true, nil)
	md, err := converter.ConvertString(string(body))
	if err != nil {
		return "", fmt.Errorf("html to md conversion failed: %w", err)
	}

	return strings.TrimSpace(md), nil
}

func fetchUrl(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := request.GetArguments()
	url := ""
	if u, ok := args["url"]; ok {
		url = fmt.Sprintf("%v", u)
	}
	if url == "" {
		return mcp.NewToolResultError("Missing or invalid 'url' parameter"), nil
	}

	markdownText, err := fetchAndConvert(url)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	return mcp.NewToolResultText(markdownText), nil
}

func httpRequest(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := request.GetArguments()
	method := args["method"].(string)
	url := args["url"].(string)
	body := ""
	if b, ok := args["body"].(string); ok {
		body = b
	}

	var req *http.Request
	var err error
	if body != "" {
		req, err = http.NewRequest(method, url, strings.NewReader(body))
	} else {
		req, err = http.NewRequest(method, url, nil)
	}
	if err != nil {
		return mcp.NewToolResultErrorFromErr("unable to create request", err), nil
	}

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return mcp.NewToolResultErrorFromErr("unable to execute request", err), nil
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(resp.Body)
		return mcp.NewToolResultError(fmt.Sprintf("HTTP %d: %s\nBody: %s", resp.StatusCode, resp.Status, string(respBody))), nil
	}

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return mcp.NewToolResultErrorFromErr("unable to read request response", err), nil
	}

	return mcp.NewToolResultText(fmt.Sprintf("Status: %d\nBody: %s", resp.StatusCode, string(respBody))), nil
}
