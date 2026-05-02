package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// =============================================================================
// MCP Structures
// =============================================================================

type Request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      interface{}     `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type Response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      interface{}     `json:"id"`
	Result  *ResponseResult `json:"result,omitempty"`
	Error   *ErrorResponse  `json:"error,omitempty"`
}

type ResponseResult struct {
	ProtocolVersion string                 `json:"protocolVersion,omitempty"`
	Capabilities    map[string]interface{} `json:"capabilities,omitempty"`
	ServerInfo      map[string]string      `json:"serverInfo,omitempty"`
	Content         []ContentBlock         `json:"content,omitempty"`
}

type ErrorResponse struct {
	Code    int         `json:"code"`
	Message string      `json:"message"`
	Data    interface{} `json:"data,omitempty"`
}

type ContentBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

const mcpVersion = "2024-11-05"

// =============================================================================
// SSE Hub (multi-client broadcaster)
// =============================================================================

type Client struct {
	ch chan []byte
}

type Hub struct {
	mu      sync.Mutex
	clients map[*Client]struct{}
}

func NewHub() *Hub {
	return &Hub{
		clients: make(map[*Client]struct{}),
	}
}

func (h *Hub) Add(c *Client) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.clients[c] = struct{}{}
}

func (h *Hub) Remove(c *Client) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.clients, c)
	close(c.ch)
}

func (h *Hub) Broadcast(msg []byte) {
	h.mu.Lock()
	defer h.mu.Unlock()

	for c := range h.clients {
		select {
		case c.ch <- msg:
		default:
			// drop if client is slow
		}
	}
}

var hub = NewHub()

// =============================================================================
// HTTP Handlers
// =============================================================================

func sseHandler(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "Streaming unsupported", http.StatusInternalServerError)
		return
	}

	client := &Client{ch: make(chan []byte, 100)}
	hub.Add(client)
	defer hub.Remove(client)

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	// keepalive
	go func() {
		ticker := time.NewTicker(20 * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			client.ch <- []byte(`: ping`)
		}
	}()

	for {
		select {
		case msg := <-client.ch:
			fmt.Fprintf(w, "data: %s\n\n", msg)
			flusher.Flush()
		case <-r.Context().Done():
			return
		}
	}
}

func rpcHandler(w http.ResponseWriter, r *http.Request) {
	defer func() {
		if r := recover(); r != nil {
			log.Println("panic:", r)
		}
	}()

	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "bad request", 400)
		return
	}

	var req Request
	if err := json.Unmarshal(body, &req); err != nil {
		writeHTTPError(w, req.ID, -32700, "parse error")
		return
	}

	resp := handleRequest(&req)

	out, _ := json.Marshal(resp)

	// respond immediately (RPC response)
	w.Header().Set("Content-Type", "application/json")
	w.Write(out)

	// ALSO push via SSE (MCP style)
	hub.Broadcast(out)
}

// =============================================================================
// Core MCP Logic
// =============================================================================

func handleRequest(req *Request) Response {
	switch req.Method {

	case "initialize":
		return Response{
			JSONRPC: "2.0",
			ID:      req.ID,
			Result: &ResponseResult{
				ProtocolVersion: mcpVersion,
				Capabilities: map[string]interface{}{
					"tools":     struct{}{},
					"resources": struct{}{},
				},
				ServerInfo: map[string]string{
					"name":    "mcp-http-sse-server",
					"version": "3.0.0",
				},
			},
		}

	case "tools/list":
		return Response{
			JSONRPC: "2.0",
			ID:      req.ID,
			Result: &ResponseResult{
				Content: []ContentBlock{
					{Type: "text", Text: "tools available"},
				},
			},
		}

	case "tools/call":
		return handleToolCall(req)

	default:
		return Response{
			JSONRPC: "2.0",
			ID:      req.ID,
			Error: &ErrorResponse{
				Code:    -32601,
				Message: "method not found",
			},
		}
	}
}

// =============================================================================
// Example Tool (minimal for demo)
// =============================================================================

func handleToolCall(req *Request) Response {
	var args struct {
		Name string `json:"name"`
	}

	json.Unmarshal(req.Params, &args)

	switch args.Name {
	case "ping":
		return Response{
			JSONRPC: "2.0",
			ID:      req.ID,
			Result: &ResponseResult{
				Content: []ContentBlock{
					{Type: "text", Text: "pong"},
				},
			},
		}
	}

	return Response{
		JSONRPC: "2.0",
		ID:      req.ID,
		Error: &ErrorResponse{
			Code:    -32601,
			Message: "tool not found",
		},
	}
}

// =============================================================================
// Helpers
// =============================================================================

func writeHTTPError(w http.ResponseWriter, id interface{}, code int, msg string) {
	resp := Response{
		JSONRPC: "2.0",
		ID:      id,
		Error: &ErrorResponse{
			Code:    code,
			Message: msg,
		},
	}
	out, _ := json.Marshal(resp)
	w.Write(out)
}

// =============================================================================
// Resource helper (kept simple)
// =============================================================================

func registerDirectoryResources(dir string) {
	filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		return nil
	})
}

// =============================================================================
// MAIN
// =============================================================================

func main() {
	log.Println("MCP HTTP+SSE server running on :8080")

	http.HandleFunc("/events", sseHandler)
	http.HandleFunc("/rpc", rpcHandler)

	log.Fatal(http.ListenAndServe(":8080", nil))
}
