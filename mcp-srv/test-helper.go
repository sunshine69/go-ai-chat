package main

import "github.com/mark3labs/mcp-go/mcp"

// makeReq builds a CallToolRequest with the given string args — no server needed.
// Pass nil for handlers that take no arguments.
func makeReq(args map[string]any) mcp.CallToolRequest {
	return mcp.CallToolRequest{
		Params: mcp.CallToolParams{
			Arguments: args,
		},
	}
}
