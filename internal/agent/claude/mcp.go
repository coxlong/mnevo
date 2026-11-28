package claude

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"

	"github.com/coxlong/mnevo/internal/domain"
	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// MCPClient exposes the local Mnevo daemon operations needed by the MCP tool server.
type MCPClient interface {
	GetReference(ctx context.Context, ref string) (domain.MemoryDetail, error)
	SubmitFeedback(ctx context.Context, in domain.FeedbackInput) (domain.ReceiptResult, error)
}

// RunMCP registers the memory tools and serves them over the given MCP transport.
func RunMCP(ctx context.Context, client MCPClient, version string, transport mcp.Transport) error {
	server := mcp.NewServer(&mcp.Implementation{Name: "mnevo", Version: version}, &mcp.ServerOptions{
		Capabilities:              &mcp.ServerCapabilities{Tools: &mcp.ToolCapabilities{}},
		SupportedProtocolVersions: []string{"2024-11-05", "2025-03-26", "2025-06-18"},
	})
	str := func(description string) map[string]any {
		return map[string]any{"type": "string", "description": description}
	}
	server.AddTool(&mcp.Tool{
		Name:        "get_memory",
		Description: "Read full details and provenance for a memory reference delivered in this task.",
		InputSchema: map[string]any{
			"type":                 "object",
			"additionalProperties": false,
			"required":             []string{"reference"},
			"properties": map[string]any{
				"reference": str("The reference in the Mnevo recall."),
			},
		},
	}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var input struct {
			Reference string `json:"reference"`
		}
		if err := domain.Decode(bytes.NewReader(req.Params.Arguments), &input); err != nil || strings.TrimSpace(input.Reference) == "" {
			return nil, &jsonrpc.Error{Code: jsonrpc.CodeInvalidParams, Message: "Expected a reference"}
		}
		res, err := client.GetReference(ctx, strings.TrimSpace(input.Reference))
		if err != nil {
			res = domain.MemoryDetail{Status: "unavailable"}
		}
		return memoryToolResult(res, res.Status)
	})

	server.AddTool(&mcp.Tool{
		Name:        "submit_memory_feedback",
		Description: "Submit feedback for a reference in this task. Reuse idempotency_key when retrying.",
		InputSchema: map[string]any{
			"type":                 "object",
			"additionalProperties": false,
			"required":             []string{"reference", "kind", "reason", "idempotency_key"},
			"properties": map[string]any{
				"reference":       str("The reference in the Mnevo recall."),
				"kind":            map[string]any{"type": "string", "enum": []string{domain.FeedbackHelpful, domain.FeedbackNotApplicable, domain.FeedbackOutdated, domain.FeedbackCorrection}},
				"reason":          str("Explain what was useful, inapplicable, or changed."),
				"evidence":        str("Optional concrete excerpt or observed fact supporting the change."),
				"idempotency_key": str("Stable caller-generated key for this submission."),
			},
		},
	}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var input domain.FeedbackInput
		if err := domain.Decode(bytes.NewReader(req.Params.Arguments), &input); err != nil || !input.Valid() {
			return nil, &jsonrpc.Error{Code: jsonrpc.CodeInvalidParams, Message: "Invalid feedback arguments"}
		}
		res, err := client.SubmitFeedback(ctx, input)
		if err != nil {
			res = domain.ReceiptResult{Status: "unavailable"}
		}
		return memoryToolResult(res, res.Status)
	})
	return server.Run(ctx, transport)
}

func memoryToolResult(payload any, status string) (*mcp.CallToolResult, error) {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: string(encoded)}},
		IsError: status != "ok" && status != "accepted",
	}, nil
}
