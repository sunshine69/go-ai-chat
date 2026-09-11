package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	"google.golang.org/api/gmail/v1"
	"google.golang.org/api/option"
)

// Scopes needed: modify covers search/read/trash (label changes), compose covers draft creation.
var gmailScopes = []string{gmail.GmailModifyScope, gmail.GmailComposeScope}

// GmailToolManager wraps an authenticated Gmail API client.
type GmailToolManager struct {
	svc *gmail.Service
}

// NewGmailToolManager builds the manager from OAuth credentials + a cached token file.
// credentialsPath: OAuth client secret JSON downloaded from Google Cloud Console.
// tokenPath: where the user's access/refresh token is cached (created on first run).
func NewGmailToolManager(ctx context.Context, credentialsPath, tokenPath string) (*GmailToolManager, error) {
	b, err := os.ReadFile(credentialsPath)
	if err != nil {
		return nil, fmt.Errorf("read credentials file %q: %w", credentialsPath, err)
	}
	config, err := google.ConfigFromJSON(b, gmailScopes...)
	if err != nil {
		return nil, fmt.Errorf("parse client secret: %w", err)
	}

	client, err := getHTTPClient(ctx, config, tokenPath)
	if err != nil {
		return nil, fmt.Errorf("get authenticated client: %w", err)
	}

	svc, err := gmail.NewService(ctx, option.WithHTTPClient(client))
	if err != nil {
		return nil, fmt.Errorf("create gmail service: %w", err)
	}

	return &GmailToolManager{svc: svc}, nil
}

func getHTTPClient(ctx context.Context, config *oauth2.Config, tokenPath string) (*http.Client, error) {
	tok, err := tokenFromFile(tokenPath)
	if err != nil {
		tok, err = tokenFromWeb(ctx, config)
		if err != nil {
			return nil, err
		}
		if err := saveToken(tokenPath, tok); err != nil {
			return nil, err
		}
	}
	return config.Client(ctx, tok), nil
}

func tokenFromFile(path string) (*oauth2.Token, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	tok := &oauth2.Token{}
	if err := json.NewDecoder(f).Decode(tok); err != nil {
		return nil, err
	}
	return tok, nil
}

func tokenFromWeb(ctx context.Context, config *oauth2.Config) (*oauth2.Token, error) {
	authURL := config.AuthCodeURL("state-token", oauth2.AccessTypeOffline)
	fmt.Printf("Go to the following link in your browser, then paste the authorization code:\n%v\n\nAuthorization code: ", authURL)

	var code string
	if _, err := fmt.Scan(&code); err != nil {
		return nil, fmt.Errorf("read auth code: %w", err)
	}
	tok, err := config.Exchange(ctx, code)
	if err != nil {
		return nil, fmt.Errorf("exchange auth code: %w", err)
	}
	return tok, nil
}

func saveToken(path string, token *oauth2.Token) error {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		return fmt.Errorf("cache token to %q: %w", path, err)
	}
	defer f.Close()
	return json.NewEncoder(f).Encode(token)
}

// ---------- tool registration ----------

func registerGmailTools(s *server.MCPServer, tool *GmailToolManager) {
	s.AddTool(mcp.NewTool("gmail_search",
		mcp.WithDescription(`Search Gmail messages using Gmail's search syntax and return matching message IDs with subject/from/date/snippet.

WRONG: query="emails from bob" (plain English is not Gmail search syntax, will return zero/wrong results)
CORRECT: query="from:bob@example.com is:unread newer_than:7d"

Common operators: from:, to:, subject:, has:attachment, is:unread, is:read, label:, after:YYYY/MM/DD, before:YYYY/MM/DD, newer_than:Nd.`),
		mcp.WithString("query", mcp.Description("Gmail search query using Gmail search operators, e.g. \"from:alice@example.com has:attachment\".")),
		mcp.WithString("max_results", mcp.Description("Max number of messages to return as a string integer (default: 10, max: 50).")),
	), tool.handleSearch)

	s.AddTool(mcp.NewTool("gmail_read_message",
		mcp.WithDescription(`Fetch a single Gmail message by ID: headers (From/To/Subject/Date), decoded plain-text body, and a list of any attachments (filename + attachment_id needed for gmail_download_attachment).

WRONG: message_id="latest" (not a real ID; call gmail_search first to get real message IDs)
CORRECT: message_id="18c9a1f2e4b3d5a7" (an ID returned by gmail_search)`),
		mcp.WithString("message_id", mcp.Description("Gmail message ID, obtained from gmail_search results.")),
	), tool.handleReadMessage)

	s.AddTool(mcp.NewTool("gmail_download_attachment",
		mcp.WithDescription(`Download a specific attachment from a message to local disk and return the saved file path.

WRONG: calling this without first running gmail_read_message to get attachment_id
CORRECT: gmail_read_message(message_id) -> note attachment_id -> gmail_download_attachment(message_id, attachment_id, filename)`),
		mcp.WithString("message_id", mcp.Description("Gmail message ID containing the attachment.")),
		mcp.WithString("attachment_id", mcp.Description("Attachment ID, from gmail_read_message output.")),
		mcp.WithString("filename", mcp.Description("Filename to save as (default: attachment's original filename, or attachment_id if unknown).")),
		mcp.WithString("output_dir", mcp.Description("Directory to save into (default: current working directory). Created if missing.")),
	), tool.handleDownloadAttachment)

	s.AddTool(mcp.NewTool("gmail_trash_message",
		mcp.WithDescription(`Move a message to Trash (recoverable for 30 days, matching Gmail UI behavior). This is NOT permanent deletion.

WRONG: message_id="all" (batch trashing is not supported, one message per call)
CORRECT: message_id="18c9a1f2e4b3d5a7"`),
		mcp.WithString("message_id", mcp.Description("Gmail message ID to move to trash.")),
	), tool.handleTrashMessage)

	s.AddTool(mcp.NewTool("gmail_create_draft",
		mcp.WithDescription(`Create a draft email in Gmail (NOT sent — saved to Drafts for the user to review and send manually). Use this whenever asked to "write", "compose", or "draft" an email.

WRONG: expecting the email to be sent automatically — it is never sent by this tool
CORRECT: draft is created; tell the user it's waiting for their review in Gmail Drafts`),
		mcp.WithString("to", mcp.Description("Recipient email address(es), comma-separated.")),
		mcp.WithString("subject", mcp.Description("Email subject line.")),
		mcp.WithString("body", mcp.Description("Plain-text email body.")),
		mcp.WithString("cc", mcp.Description("Optional CC address(es), comma-separated (default: none).")),
	), tool.handleCreateDraft)
}

// ---------- handlers ----------

func (t *GmailToolManager) handleSearch(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	query := req.GetString("query", "")
	if query == "" {
		return mcp.NewToolResultError("query is required, e.g. \"from:alice@example.com is:unread\""), nil
	}
	maxResults := req.GetInt("max_results", 10)
	if maxResults <= 0 || maxResults > 50 {
		maxResults = 10
	}

	resp, err := t.svc.Users.Messages.List("me").Q(query).MaxResults(int64(maxResults)).Context(ctx).Do()
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("gmail search failed: %v", err)), nil
	}
	if len(resp.Messages) == 0 {
		return mcp.NewToolResultText("No messages matched that query."), nil
	}

	var sb strings.Builder
	for _, m := range resp.Messages {
		msg, err := t.svc.Users.Messages.Get("me", m.Id).Format("metadata").
			MetadataHeaders("Subject", "From", "Date").Context(ctx).Do()
		if err != nil {
			fmt.Fprintf(&sb, "id=%s (failed to fetch headers: %v)\n", m.Id, err)
			continue
		}
		headers := headerMap(msg.Payload.Headers)
		fmt.Fprintf(&sb, "id=%s | from=%s | subject=%s | date=%s | snippet=%s\n",
			m.Id, headers["From"], headers["Subject"], headers["Date"], msg.Snippet)
	}
	return mcp.NewToolResultText(sb.String()), nil
}

func (t *GmailToolManager) handleReadMessage(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	id := req.GetString("message_id", "")
	if id == "" {
		return mcp.NewToolResultError("message_id is required (get one from gmail_search)"), nil
	}

	msg, err := t.svc.Users.Messages.Get("me", id).Format("full").Context(ctx).Do()
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("failed to fetch message %q: %v", id, err)), nil
	}

	headers := headerMap(msg.Payload.Headers)
	body, attachments := walkParts(msg.Payload)

	var sb strings.Builder
	fmt.Fprintf(&sb, "From: %s\nTo: %s\nSubject: %s\nDate: %s\n\n", headers["From"], headers["To"], headers["Subject"], headers["Date"])
	if body == "" {
		sb.WriteString("(no plain-text body found)\n")
	} else {
		sb.WriteString(body)
		sb.WriteString("\n")
	}
	if len(attachments) > 0 {
		sb.WriteString("\nAttachments:\n")
		for _, a := range attachments {
			fmt.Fprintf(&sb, "  - filename=%q attachment_id=%s size=%d bytes\n", a.filename, a.attachmentID, a.size)
		}
	}
	return mcp.NewToolResultText(sb.String()), nil
}

func (t *GmailToolManager) handleDownloadAttachment(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	messageID := req.GetString("message_id", "")
	attachmentID := req.GetString("attachment_id", "")
	if messageID == "" || attachmentID == "" {
		return mcp.NewToolResultError("both message_id and attachment_id are required (get them from gmail_read_message)"), nil
	}
	filename := req.GetString("filename", attachmentID)
	outputDir := req.GetString("output_dir", ".")

	att, err := t.svc.Users.Messages.Attachments.Get("me", messageID, attachmentID).Context(ctx).Do()
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("failed to fetch attachment: %v", err)), nil
	}

	data, err := base64.URLEncoding.WithPadding(base64.NoPadding).DecodeString(att.Data)
	if err != nil {
		// Gmail sometimes returns padded base64url; retry with padding.
		data, err = base64.URLEncoding.DecodeString(att.Data)
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("failed to decode attachment data: %v", err)), nil
		}
	}

	if err := os.MkdirAll(outputDir, 0755); err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("failed to create output dir %q: %v", outputDir, err)), nil
	}
	outPath := filepath.Join(outputDir, filepath.Base(filename))
	if err := os.WriteFile(outPath, data, 0644); err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("failed to write file %q: %v", outPath, err)), nil
	}

	return mcp.NewToolResultText(fmt.Sprintf("Saved attachment to %s (%d bytes)", outPath, len(data))), nil
}

func (t *GmailToolManager) handleTrashMessage(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	id := req.GetString("message_id", "")
	if id == "" {
		return mcp.NewToolResultError("message_id is required (get one from gmail_search)"), nil
	}

	if _, err := t.svc.Users.Messages.Trash("me", id).Context(ctx).Do(); err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("failed to trash message %q: %v", id, err)), nil
	}
	return mcp.NewToolResultText(fmt.Sprintf("Message %s moved to Trash.", id)), nil
}

func (t *GmailToolManager) handleCreateDraft(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	to := req.GetString("to", "")
	subject := req.GetString("subject", "")
	body := req.GetString("body", "")
	cc := req.GetString("cc", "")

	if to == "" || subject == "" || body == "" {
		return mcp.NewToolResultError("to, subject, and body are all required"), nil
	}

	raw := buildRawMessage(to, cc, subject, body)
	draft := &gmail.Draft{Message: &gmail.Message{Raw: raw}}

	created, err := t.svc.Users.Drafts.Create("me", draft).Context(ctx).Do()
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("failed to create draft: %v", err)), nil
	}

	return mcp.NewToolResultText(fmt.Sprintf("Draft created (id=%s). It has NOT been sent — review and send it from Gmail Drafts.", created.Id)), nil
}

// ---------- MIME helpers ----------

type attachmentInfo struct {
	filename     string
	attachmentID string
	size         int64
}

// walkParts recursively finds the first text/plain body and all attachment parts.
func walkParts(part *gmail.MessagePart) (body string, attachments []attachmentInfo) {
	if part == nil {
		return "", nil
	}

	if part.Filename != "" && part.Body != nil && part.Body.AttachmentId != "" {
		attachments = append(attachments, attachmentInfo{
			filename:     part.Filename,
			attachmentID: part.Body.AttachmentId,
			size:         part.Body.Size,
		})
	} else if part.MimeType == "text/plain" && part.Body != nil && part.Body.Data != "" && body == "" {
		decoded, err := base64.URLEncoding.DecodeString(part.Body.Data)
		if err != nil {
			decoded, _ = base64.RawURLEncoding.DecodeString(part.Body.Data)
		}
		body = string(decoded)
	}

	for _, child := range part.Parts {
		childBody, childAttachments := walkParts(child)
		if body == "" {
			body = childBody
		}
		attachments = append(attachments, childAttachments...)
	}
	return body, attachments
}

func headerMap(headers []*gmail.MessagePartHeader) map[string]string {
	m := make(map[string]string, len(headers))
	for _, h := range headers {
		m[h.Name] = h.Value
	}
	return m
}

// buildRawMessage constructs an RFC 2822 message and base64url-encodes it for the Gmail API's Raw field.
func buildRawMessage(to, cc, subject, body string) string {
	var msg strings.Builder
	fmt.Fprintf(&msg, "To: %s\r\n", to)
	if cc != "" {
		fmt.Fprintf(&msg, "Cc: %s\r\n", cc)
	}
	fmt.Fprintf(&msg, "Subject: %s\r\n", subject)
	msg.WriteString("MIME-Version: 1.0\r\n")
	msg.WriteString("Content-Type: text/plain; charset=\"UTF-8\"\r\n\r\n")
	msg.WriteString(body)
	return base64.URLEncoding.EncodeToString([]byte(msg.String()))
}
