// Confluence MCP Proxy
// Provides MCP tools to interact with Confluence Cloud API
// This allows the LLM to search and retrieve Confluence pages on the fly

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"

	html2md "github.com/JohannesKaufmann/html-to-markdown"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// ConfluenceAPI represents the Confluence Cloud/Data Center API configuration
type ConfluenceAPI struct {
	BaseURL    string
	APIKey     string
	Username   string
	AuthMode   string // "basic" (Cloud: email+API token) or "bearer" (Data Center/Server: Personal Access Token)
	HTTPClient *http.Client
}

// ConfluenceBody represents the body storage format
type ConfluenceBody struct {
	Storage ConfluenceBodyStorage `json:"storage"`
}

// ConfluenceBodyStorage represents the storage format
type ConfluenceBodyStorage struct {
	Value          string `json:"value"`
	Representation string `json:"representation"`
}

// ConfluenceSpace represents a Confluence space (v1 REST API shape)
type ConfluenceSpace struct {
	ID          json.Number           `json:"id"`
	Key         string                `json:"key"`
	Name        string                `json:"name"`
	Description ConfluenceDescription `json:"description"`
}

// ConfluenceDescription holds the (optionally expanded) plain-text space description
type ConfluenceDescription struct {
	Plain struct {
		Value string `json:"value"`
	} `json:"plain"`
}

// ConfluenceVersion represents the version object returned by the v1 REST API (not a plain int)
type ConfluenceVersion struct {
	Number int `json:"number"`
}

// ConfluenceHistory carries page creation metadata (only present when expand=history is requested)
type ConfluenceHistory struct {
	CreatedDate string `json:"createdDate"`
}

// ConfluenceSearchResult represents a search result from Confluence
type ConfluenceSearchResult struct {
	Results   []ConfluencePageResult `json:"results"`
	TotalSize int                    `json:"totalSize"`
	Start     int                    `json:"start"`
	Limit     int                    `json:"limit"`
}

// ConfluencePageResult represents a search result page
type ConfluencePageResult struct {
	ID      string            `json:"id"`
	Title   string            `json:"title"`
	Space   ConfluenceSpace   `json:"space"`
	Body    ConfluenceBody    `json:"body"`
	History ConfluenceHistory `json:"history"`
	Version ConfluenceVersion `json:"version"`
}

// NewConfluenceAPI creates a new Confluence API client
func NewConfluenceAPI() (*ConfluenceAPI, error) {
	baseURL := os.Getenv("CONFLUENCE_BASE_URL")
	apiKey := os.Getenv("CONFLUENCE_API_TOKEN")
	username := os.Getenv("CONFLUENCE_USERNAME")
	if baseURL == "" || apiKey == "" || username == "" {
		return nil, fmt.Errorf("[ERROR] must set all env vars CONFLUENCE_BASE_URL CONFLUENCE_API_TOKEN CONFLUENCE_USERNAME")
	}
	// Cloud (*.atlassian.net) uses Basic auth (email + API token). Data Center/Server
	// instances use a Personal Access Token via Bearer auth instead. Default to basic
	// for backward compatibility; set CONFLUENCE_AUTH_MODE=bearer for Data Center/Server.
	authMode := strings.ToLower(os.Getenv("CONFLUENCE_AUTH_MODE"))
	if authMode == "" {
		authMode = "basic"
	}

	usingPlaceholder := baseURL == ""
	if usingPlaceholder {
		baseURL = "https://your-org.atlassian.net/wiki"
	}

	// Never log apiKey itself — only whether it was picked up from the environment.
	log.Printf("[Confluence] config: base_url=%s (from_env=%t) username=%s auth_mode=%s api_token_set=%t", baseURL, !usingPlaceholder, username, authMode, apiKey != "")
	if usingPlaceholder {
		log.Printf("[Confluence] WARNING: CONFLUENCE_BASE_URL is not set — falling back to placeholder URL, requests will fail")
	}

	return &ConfluenceAPI{
		BaseURL:    baseURL,
		APIKey:     apiKey,
		Username:   username,
		AuthMode:   authMode,
		HTTPClient: &http.Client{},
	}, nil
}

func (c *ConfluenceAPI) createRequest(method, path string) *http.Request {
	url := c.BaseURL + path
	req, _ := http.NewRequest(method, url, nil)
	if c.AuthMode == "bearer" {
		req.Header.Set("Authorization", "Bearer "+c.APIKey)
	} else {
		req.SetBasicAuth(c.Username, c.APIKey)
	}
	req.Header.Set("Accept", "application/json")
	return req
}

func (c *ConfluenceAPI) doRequest(req *http.Request) ([]byte, error) {
	log.Printf("[Confluence] -> %s %s", req.Method, req.URL.String())
	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		log.Printf("[Confluence] <- request failed: %v", err)
		return nil, err
	}
	defer resp.Body.Close()
	log.Printf("[Confluence] <- %d %s", resp.StatusCode, req.URL.String())

	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("Confluence API error %d: %s", resp.StatusCode, string(body))
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	// Confluence sometimes returns HTML error pages instead of JSON
	if len(body) > 0 && body[0] == '<' {
		return nil, fmt.Errorf("Confluence API returned HTML error page: %s", string(body))
	}

	return body, nil
}

// searchPages searches for pages in Confluence
func (c *ConfluenceAPI) searchPages(cql string) (*ConfluenceSearchResult, error) {
	path := fmt.Sprintf("/rest/api/content/search?cql=%s&expand=body.storage,version,space,history",
		url.QueryEscape(cql))

	req := c.createRequest("GET", path)
	body, err := c.doRequest(req)
	if err != nil {
		return nil, err
	}

	var result ConfluenceSearchResult
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("failed to parse search results: %w", err)
	}

	return &result, nil
}

// getPage retrieves a specific page by ID with its body
func (c *ConfluenceAPI) getPage(pageID string) (*ConfluencePageResult, error) {
	path := fmt.Sprintf("/rest/api/content/%s?expand=body.storage,version,space,history",
		url.QueryEscape(pageID))

	req := c.createRequest("GET", path)
	body, err := c.doRequest(req)
	if err != nil {
		return nil, err
	}

	var result ConfluencePageResult
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("failed to parse page: %w", err)
	}

	return &result, nil
}

// getSpaces retrieves spaces from Confluence
func (c *ConfluenceAPI) getSpaces() ([]ConfluenceSpace, error) {
	path := "/rest/api/space?expand=description.plain"
	req := c.createRequest("GET", path)
	body, err := c.doRequest(req)
	if err != nil {
		return nil, err
	}

	var result struct {
		Spaces []ConfluenceSpace `json:"results"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("failed to parse spaces: %w", err)
	}

	return result.Spaces, nil
}

// searchByLabel searches for pages by label
func (c *ConfluenceAPI) searchByLabel(label string) (*ConfluenceSearchResult, error) {
	cql := fmt.Sprintf("label=\"%s\"", label)
	return c.searchPages(cql)
}

// Convert Confluence Storage format to plain text/markdown
func convertToText(body ConfluenceBody) string {
	// Confluence body can be in storage (html), view, or export format.
	// Storage/view/export_view are all HTML — reuse the same html2md converter as fetch_url.
	switch body.Storage.Representation {
	case "storage", "view", "export_view":
		converter := html2md.NewConverter("", true, nil)
		md, err := converter.ConvertString(body.Storage.Value)
		if err != nil {
			log.Printf("[Confluence] html to markdown conversion failed: %v", err)
			return body.Storage.Value
		}
		return strings.TrimSpace(md)
	default:
		return body.Storage.Value
	}
}

// Confluence MCP Tool Handlers

func handleConfluenceSearch(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	api, err := NewConfluenceAPI()
	if err != nil {
		return nil, err
	}
	// Support both 'query' and 'keyword' parameter names for compatibility
	query := req.GetString("query", "")
	if query == "" {
		query = req.GetString("keyword", "")
	}
	limit := req.GetInt("limit", 10)
	// Optional comma-separated Confluence space keys to scope the search (e.g. domain -> space mapping)
	spaceParam := strings.TrimSpace(req.GetString("space", ""))

	if query == "" {
		return mcp.NewToolResultError("Missing required parameter: query (or keyword)"), nil
	}

	// Search for pages matching the query, optionally scoped to one or more spaces
	cql := fmt.Sprintf("text~\"%s\"", query)
	if spaceParam != "" {
		keys := []string{}
		for _, k := range strings.Split(spaceParam, ",") {
			if k = strings.TrimSpace(k); k != "" {
				keys = append(keys, fmt.Sprintf("%q", k))
			}
		}
		if len(keys) > 0 {
			cql = fmt.Sprintf("space in (%s) and %s", strings.Join(keys, ","), cql)
		}
	}
	results, err := api.searchPages(cql)
	if err != nil {
		log.Printf("[Confluence] search query=%q space=%q failed: %v", query, spaceParam, err)
		return mcp.NewToolResultError(err.Error()), nil
	}
	log.Printf("[Confluence] search query=%q space=%q -> %d result(s) (total %d)", query, spaceParam, len(results.Results), results.TotalSize)

	if len(results.Results) == 0 {
		return mcp.NewToolResultText(fmt.Sprintf("No Confluence pages found matching: %s\n\nTry searching with different keywords or use a label-based search.", query)), nil
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Found %d Confluence pages:\n\n", results.TotalSize))

	for i, result := range results.Results {
		if i >= limit {
			break
		}
		sb.WriteString(fmt.Sprintf("## %s (ID: %s)\n", result.Title, result.ID))
		sb.WriteString(fmt.Sprintf("Space: %s\n", result.Space.Name))
		sb.WriteString(fmt.Sprintf("Last updated: %s\n\n", result.History.CreatedDate))
	}

	return mcp.NewToolResultText(sb.String()), nil
}

func handleConfluenceReadPage(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	api, err := NewConfluenceAPI()
	if err != nil {
		return nil, err
	}
	pageID := req.GetString("page_id", "")

	if pageID == "" {
		return mcp.NewToolResultError("Missing required parameter: page_id"), nil
	}

	page, err := api.getPage(pageID)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("# %s\n\n", page.Title))
	sb.WriteString(fmt.Sprintf("Space: %s\n", page.Space.Name))
	sb.WriteString(fmt.Sprintf("Page ID: %s\n", page.ID))
	sb.WriteString(fmt.Sprintf("Last updated: %s\n\n", page.History.CreatedDate))

	text := convertToText(page.Body)
	sb.WriteString(text)

	return mcp.NewToolResultText(sb.String()), nil
}

func handleConfluenceGetSpaces(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	api, err := NewConfluenceAPI()
	if err != nil {
		return nil, err
	}
	spaces, err := api.getSpaces()
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	var sb strings.Builder
	sb.WriteString("Confluence Spaces:\n\n")

	for _, space := range spaces {
		sb.WriteString(fmt.Sprintf("### %s (Key: %s, ID: %s)\n", space.Name, space.Key, space.ID))
		sb.WriteString(space.Description.Plain.Value + "\n\n")
	}

	return mcp.NewToolResultText(sb.String()), nil
}

func handleConfluenceSearchByLabel(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	api, err := NewConfluenceAPI()
	if err != nil {
		return nil, err
	}
	label := req.GetString("label", "")
	limit := req.GetInt("limit", 10)

	if label == "" {
		return mcp.NewToolResultError("Missing required parameter: label"), nil
	}

	results, err := api.searchByLabel(label)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Pages with label '%s':\n\n", label))

	if len(results.Results) == 0 {
		sb.WriteString("No pages found with this label.\n")
		return mcp.NewToolResultText(sb.String()), nil
	}

	for i, result := range results.Results {
		if i >= limit {
			break
		}
		sb.WriteString(fmt.Sprintf("## %s (ID: %s)\n", result.Title, result.ID))
		sb.WriteString(fmt.Sprintf("Space: %s\n\n", result.Space.Name))
	}

	return mcp.NewToolResultText(sb.String()), nil
}

// RegisterConfluenceTools registers Confluence MCP tools
func RegisterConfluenceTools(s *server.MCPServer) error {
	// Check if Confluence is configured
	baseURL := os.Getenv("CONFLUENCE_BASE_URL")
	if baseURL == "" {
		return fmt.Errorf("confluence base URL not configured — set CONFLUENCE_BASE_URL environment variable")
	}
	log.Printf("[Confluence] tools registered: confluence_search, confluence_read_page, confluence_get_spaces, confluence_search_by_label (base_url=%s)", baseURL)

	s.AddTool(mcp.NewTool("confluence_search",
		mcp.WithDescription("Search for pages in Confluence Cloud/Data Center. Returns a list of matching pages with titles, space names, and IDs. Use the page_id from this tool to read full content."),
		mcp.WithString("query", mcp.Required(), mcp.Description("Search query to find Confluence pages.")),
		mcp.WithString("space", mcp.Description("Optional comma-separated Confluence space key(s) to scope the search to (e.g. 'TS' or 'PMP,DPAI').")),
		mcp.WithNumber("limit", mcp.Description("Maximum number of results to return. Default: 10.")),
	), handleConfluenceSearch)

	s.AddTool(mcp.NewTool("confluence_read_page",
		mcp.WithDescription("Read the full content of a Confluence page by its page ID. Returns the page title, space, and converted text content."),
		mcp.WithString("page_id", mcp.Required(), mcp.Description("The Confluence page ID to read. Get this from confluence_search results.")),
	), handleConfluenceReadPage)

	s.AddTool(mcp.NewTool("confluence_get_spaces",
		mcp.WithDescription("List all Confluence spaces with their names, keys, and descriptions."),
	), handleConfluenceGetSpaces)

	s.AddTool(mcp.NewTool("confluence_search_by_label",
		mcp.WithDescription("Search for Confluence pages by label. Returns pages that have the specified label."),
		mcp.WithString("label", mcp.Required(), mcp.Description("The Confluence label to search for.")),
		mcp.WithNumber("limit", mcp.Description("Maximum number of results to return. Default: 10.")),
	), handleConfluenceSearchByLabel)

	return nil
}
