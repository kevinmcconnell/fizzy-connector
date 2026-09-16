package mcpserver

import (
	"context"
	"encoding/json"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kevinmcconnell/fizzy-connector/internal/claude"
	"github.com/kevinmcconnell/fizzy-connector/internal/ipc"
)

type approveResult struct {
	Behavior     string         `json:"behavior"`
	UpdatedInput map[string]any `json:"updatedInput,omitempty"`
	Message      string         `json:"message,omitempty"`
}

func (s *Server) addApprovalTool(server *mcp.Server) {
	mcp.AddTool(server, &mcp.Tool{
		Name:        claude.ApprovalToolName,
		Description: "Used by Claude Code to ask for permission. Do not call this tool yourself.",
	}, s.approve)
}

// approve gets tool_name, input and tool_use_id from Claude Code. The input
// type is a map so that a new field does not fail the schema validation.
func (s *Server) approve(_ context.Context, _ *mcp.CallToolRequest, in map[string]any) (*mcp.CallToolResult, any, error) {
	toolName, _ := in["tool_name"].(string)
	input, _ := in["input"].(map[string]any)
	toolInput, _ := json.Marshal(input)
	wait := s.cfg.ApprovalTimeout.Duration + time.Minute

	result := approveResult{Behavior: "deny"}
	response, err := ipc.SendAndWait(s.socket, ipc.Request{
		Op:        ipc.OpApprove,
		Token:     s.token,
		ToolName:  toolName,
		ToolInput: toolInput,
	}, wait)
	switch {
	case err != nil:
		result.Message = "The permission request failed: " + err.Error()
	case response.Approved:
		result = approveResult{Behavior: "allow", UpdatedInput: input}
	default:
		result.Message = response.Message
	}

	encoded, _ := json.Marshal(result)
	return text("%s", encoded), nil, nil
}
