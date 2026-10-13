package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
)

// MCPRequest represents a generic MCP JSON-RPC request
type MCPRequest struct {
	JSONRPC string                 `json:"jsonrpc"`
	Method  string                 `json:"method"`
	Params  map[string]interface{} `json:"params,omitempty"`
	ID      interface{}            `json:"id,omitempty"`
}

// MCPResponse represents a generic MCP JSON-RPC response
type MCPResponse struct {
	JSONRPC string                   `json:"jsonrpc"`
	Result  json.RawMessage          `json:"result,omitempty"`
	Error   *MCPError                `json:"error,omitempty"`
	ID      interface{}              `json:"id,omitempty"`
}

type MCPError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func main() {
	if len(os.Args) < 2 {
		fmt.Println("Usage: test-client-session <server-base-url>")
		fmt.Println("Example: test-client-session http://localhost:8081")
		os.Exit(1)
	}

	baseURL := os.Args[1]

	fmt.Printf("Testing MCP Server at: %s\n\n", baseURL)

	// Test 1: Create session with 'testworkdir'
	fmt.Println("=== Test 1: Create session with 'testworkdir' ===")
	sessionID1, err := createSession(baseURL + "/mcp/testworkdir")
	if err != nil {
		fmt.Printf("ERROR: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("Session ID 1: %s\n\n", sessionID1)

	// Test 2: Create session with 'anotherworkdir'
	fmt.Println("=== Test 2: Create session with 'anotherworkdir' ===")
	sessionID2, err := createSession(baseURL + "/mcp/anotherworkdir")
	if err != nil {
		fmt.Printf("ERROR: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("Session ID 2: %s\n\n", sessionID2)

	// Test 3: Try to access 'testworkdir' again with same session (should work, but workdir is already occupied)
	// Actually, the workdir manager should reject new access to an occupied workdir
	fmt.Println("=== Test 3: Try to create another session with 'testworkdir' (should be rejected) ===")
	_, err = createSession(baseURL + "/mcp/testworkdir")
	if err != nil {
		fmt.Printf("ERROR (expected): %v\n", err)
	} else {
		fmt.Printf("Response: Session created (this might not be rejected depending on implementation)\n")
	}

	// Test 4: Use session 1 to list files in testworkdir
	fmt.Println("\n=== Test 4: List files in 'testworkdir' using session 1 ===")
	resp, err := callMCPWithSession(baseURL+"/mcp/testworkdir", sessionID1, createListDirectoryRequest("."))
	if err != nil {
		fmt.Printf("ERROR: %v\n", err)
	} else {
		fmt.Printf("Response: %s\n", string(resp))
	}

	// Test 5: Use session 2 to list files in anotherworkdir
	fmt.Println("\n=== Test 5: List files in 'anotherworkdir' using session 2 ===")
	resp, err = callMCPWithSession(baseURL+"/mcp/anotherworkdir", sessionID2, createListDirectoryRequest("."))
	if err != nil {
		fmt.Printf("ERROR: %v\n", err)
	} else {
		fmt.Printf("Response: %s\n", string(resp))
	}

	// Test 6: Create a file in testworkdir using session 1
	fmt.Println("\n=== Test 6: Create a file in 'testworkdir' using session 1 ===")
	resp, err = callMCPWithSession(baseURL+"/mcp/testworkdir", sessionID1, createCreateFileRequest("testfile.txt", "Hello from testworkdir!"))
	if err != nil {
		fmt.Printf("ERROR: %v\n", err)
	} else {
		fmt.Printf("Response: %s\n", string(resp))
	}

	// Test 7: Read the file in testworkdir using session 1
	fmt.Println("\n=== Test 7: Read file in 'testworkdir' using session 1 ===")
	resp, err = callMCPWithSession(baseURL+"/mcp/testworkdir", sessionID1, createReadFileRequest("testfile.txt"))
	if err != nil {
		fmt.Printf("ERROR: %v\n", err)
	} else {
		fmt.Printf("Response: %s\n", string(resp))
	}

	fmt.Println("\n=== All tests completed ===")
}

func createSession(url string) (string, error) {
	client := &http.Client{Timeout: 10 * time.Second}

	// Initial request to establish session
	reqBody := MCPRequest{
		JSONRPC: "2.0",
		Method:  "initialize",
		Params: map[string]interface{}{
			"protocolVersion": "2024-11-05",
			"clientInfo": map[string]interface{}{
				"name":    "test-client",
				"version": "1.0.0",
			},
		},
		ID: 1,
	}

	body, err := json.Marshal(reqBody)
	if err != nil {
		return "", err
	}

	httpReq, err := http.NewRequest("POST", url, bytes.NewBuffer(body))
	if err != nil {
		return "", err
	}

	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(httpReq)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(respBody))
	}

	// Extract session ID from response headers
	sessionID := resp.Header.Get("Mcp-Session-Id")
	if sessionID == "" {
		return "", fmt.Errorf("No Mcp-Session-Id in response")
	}

	return sessionID, nil
}

func callMCPWithSession(url, sessionID string, req MCPRequest) ([]byte, error) {
	client := &http.Client{Timeout: 10 * time.Second}

	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}

	httpReq, err := http.NewRequest("POST", url, bytes.NewBuffer(body))
	if err != nil {
		return nil, err
	}

	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Mcp-Session-Id", sessionID)

	resp, err := client.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode != http.StatusOK {
		return respBody, fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(respBody))
	}

	return respBody, nil
}

func createListDirectoryRequest(path string) MCPRequest {
	return MCPRequest{
		JSONRPC: "2.0",
		Method:  "tools/call",
		Params: map[string]interface{}{
			"tool_name": "list_directory",
			"arguments": map[string]interface{}{
				"path": path,
			},
		},
		ID: 1,
	}
}

func createCreateFileRequest(path, content string) MCPRequest {
	return MCPRequest{
		JSONRPC: "2.0",
		Method:  "tools/call",
		Params: map[string]interface{}{
			"tool_name": "create_new_file",
			"arguments": map[string]interface{}{
				"path":    path,
				"content": content,
			},
		},
		ID: 2,
	}
}

func createReadFileRequest(path string) MCPRequest {
	return MCPRequest{
		JSONRPC: "2.0",
		Method:  "tools/call",
		Params: map[string]interface{}{
			"tool_name": "read_file",
			"arguments": map[string]interface{}{
				"path": path,
			},
		},
		ID: 3,
	}
}