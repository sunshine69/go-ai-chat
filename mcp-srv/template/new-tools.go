package main

import (
	"context"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// This is template, use that to create a specific tool, replace NewToolManager with name u want and implementation details in the same file
type NewToolManager struct {
}

func (n *NewToolManager) handleSomething(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	// do something here
}

func registerPostgresTools(s *server.MCPServer, tool *NewToolManager) {
	// postgres_connect
	s.AddTool(mcp.NewTool("tool_name",
		mcp.WithDescription("tool desc."),
		mcp.WithString("tool_arg_name",
			mcp.Description(`tool arg description.`),
		),
		mcp.WithString("more_args", mcp.Description("more desc (default: xxx)")),
	), NewToolManager.handleSomething)
}
