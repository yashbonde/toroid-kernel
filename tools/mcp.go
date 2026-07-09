package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"charm.land/fantasy"
	mcpclient "github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/client/transport"
	"github.com/mark3labs/mcp-go/mcp"
)

// MCPServerConfig points the kernel at one remote MCP server. Its tools are
// discovered at connect time and registered into the kernel's tool registry.
type MCPServerConfig struct {
	// Name prefixes every tool from this server as "<name>__<tool>" so tools
	// from different servers can't collide. Defaults to the server's own
	// name (from its initialize response) if left empty.
	Name string
	// BaseURL is the server's MCP endpoint, e.g. "http://localhost:7842/mcp".
	BaseURL string
	// Headers are sent on every request to BaseURL — e.g. an OAuth bearer
	// token for servers that require authentication ("Authorization": "Bearer <token>").
	Headers map[string]string
}

// ConnectMCPServer connects to an MCP server, lists its tools, and registers
// each one into reg as a ToolDef that proxies calls back to the server over
// the same connection. It first tries streamable HTTP, then falls back to
// legacy SSE if the server rejects the initialize request with a 4xx.
// The returned client stays open for the life of the kernel — callers are
// responsible for closing it (the kernel does this in Close).
func ConnectMCPServer(ctx context.Context, reg *Registry, cfg MCPServerConfig) (*mcpclient.Client, error) {
	c, err := connectStreamableHTTP(ctx, reg, cfg)
	if err == nil {
		return c, nil
	}
	if errors.Is(err, transport.ErrLegacySSEServer) {
		return connectSSE(ctx, reg, cfg)
	}
	return nil, err
}

func connectStreamableHTTP(ctx context.Context, reg *Registry, cfg MCPServerConfig) (*mcpclient.Client, error) {
	var opts []transport.StreamableHTTPCOption
	if len(cfg.Headers) > 0 {
		opts = append(opts, transport.WithHTTPHeaders(cfg.Headers))
	}
	c, err := mcpclient.NewStreamableHttpClient(cfg.BaseURL, opts...)
	if err != nil {
		return nil, fmt.Errorf("mcp %s: %w", cfg.BaseURL, err)
	}
	if err := c.Start(ctx); err != nil {
		c.Close()
		return nil, fmt.Errorf("mcp %s: start: %w", cfg.BaseURL, err)
	}
	initRes, err := c.Initialize(ctx, mcp.InitializeRequest{
		Params: mcp.InitializeParams{
			ProtocolVersion: mcp.LATEST_PROTOCOL_VERSION,
			ClientInfo:      mcp.Implementation{Name: "toroid-kernel", Version: "0.1.0"},
		},
	})
	if err != nil {
		c.Close()
		return nil, fmt.Errorf("mcp %s: initialize: %w", cfg.BaseURL, err)
	}
	return finishConnection(ctx, reg, cfg, c, initRes)
}

func connectSSE(ctx context.Context, reg *Registry, cfg MCPServerConfig) (*mcpclient.Client, error) {
	var opts []transport.ClientOption
	if len(cfg.Headers) > 0 {
		opts = append(opts, transport.WithHeaders(cfg.Headers))
	}
	c, err := mcpclient.NewSSEMCPClient(cfg.BaseURL, opts...)
	if err != nil {
		return nil, fmt.Errorf("mcp %s (sse): %w", cfg.BaseURL, err)
	}
	if err := c.Start(ctx); err != nil {
		c.Close()
		return nil, fmt.Errorf("mcp %s (sse): start: %w", cfg.BaseURL, err)
	}
	initRes, err := c.Initialize(ctx, mcp.InitializeRequest{
		Params: mcp.InitializeParams{
			ProtocolVersion: mcp.LATEST_PROTOCOL_VERSION,
			ClientInfo:      mcp.Implementation{Name: "toroid-kernel", Version: "0.1.0"},
		},
	})
	if err != nil {
		c.Close()
		return nil, fmt.Errorf("mcp %s (sse): initialize: %w", cfg.BaseURL, err)
	}
	return finishConnection(ctx, reg, cfg, c, initRes)
}

func finishConnection(
	ctx context.Context,
	reg *Registry,
	cfg MCPServerConfig,
	c *mcpclient.Client,
	initRes *mcp.InitializeResult,
) (*mcpclient.Client, error) {
	name := cfg.Name
	if name == "" && initRes != nil {
		name = initRes.ServerInfo.Name
	}

	toolsRes, err := c.ListTools(ctx, mcp.ListToolsRequest{})
	if err != nil {
		c.Close()
		return nil, fmt.Errorf("mcp %s: list tools: %w", cfg.BaseURL, err)
	}

	for _, t := range toolsRes.Tools {
		reg.Register(newMCPToolDef(c, name, t))
	}

	return c, nil
}

func newMCPToolDef(c *mcpclient.Client, serverName string, t mcp.Tool) *ToolDef {
	name := t.Name
	if serverName != "" {
		name = serverName + "__" + t.Name
	}

	properties := t.InputSchema.Properties
	if properties == nil {
		properties = map[string]any{}
	}
	required := t.InputSchema.Required
	if required == nil {
		required = []string{}
	}

	agentTool := &mcpAgentTool{
		info: fantasy.ToolInfo{
			Name:        name,
			Description: t.Description,
			Parameters:  properties,
			Required:    required,
		},
		call: func(ctx context.Context, args map[string]any) (fantasy.ToolResponse, error) {
			res, err := c.CallTool(ctx, mcp.CallToolRequest{
				Params: mcp.CallToolParams{Name: t.Name, Arguments: args},
			})
			if err != nil {
				return fantasy.NewTextErrorResponse(fmt.Sprintf("Error: %v", err)), nil
			}
			return mcpResultToResponse(res), nil
		},
	}

	return &ToolDef{
		Name:        name,
		Description: t.Description,
		AgentTool:   agentTool,
	}
}

// mcpAgentTool adapts one MCP tool to fantasy.AgentTool. It's implemented by
// hand rather than via fantasy.NewAgentTool because an MCP tool's input
// schema is discovered at runtime (from the server), not known as a Go type
// at compile time for reflection-based schema generation.
type mcpAgentTool struct {
	info            fantasy.ToolInfo
	call            func(ctx context.Context, args map[string]any) (fantasy.ToolResponse, error)
	providerOptions fantasy.ProviderOptions
}

func (t *mcpAgentTool) Info() fantasy.ToolInfo { return t.info }

func (t *mcpAgentTool) Run(ctx context.Context, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
	var args map[string]any
	if call.Input != "" {
		if err := json.Unmarshal([]byte(call.Input), &args); err != nil {
			return fantasy.NewTextErrorResponse(fmt.Sprintf("invalid parameters: %v", err)), nil
		}
	}
	return t.call(ctx, args)
}

func (t *mcpAgentTool) ProviderOptions() fantasy.ProviderOptions { return t.providerOptions }

func (t *mcpAgentTool) SetProviderOptions(opts fantasy.ProviderOptions) { t.providerOptions = opts }

// mcpResultToResponse flattens an MCP CallToolResult's text content into a
// single tool response. Non-text content (images, embedded resources) is
// dropped for now — toroid's tool response type doesn't yet support
// multi-part media from an arbitrary MCP result.
func mcpResultToResponse(res *mcp.CallToolResult) fantasy.ToolResponse {
	var sb strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(mcp.TextContent); ok {
			if sb.Len() > 0 {
				sb.WriteString("\n")
			}
			sb.WriteString(tc.Text)
		}
	}
	return fantasy.ToolResponse{Type: "text", Content: sb.String(), IsError: res.IsError}
}
