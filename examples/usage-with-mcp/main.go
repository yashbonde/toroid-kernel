// Pattern: MCP CLIENT — connect the kernel to a remote MCP server, using
// Slack's hosted MCP server as the concrete target.
//
// toroid.Config.MCPServers points the kernel at one or more remote Model
// Context Protocol servers (streamable HTTP). At startup the kernel connects,
// negotiates the protocol, lists the server's tools, and registers each one
// into k.Tools as "<Name>__<tool>" — after that they're ordinary tools the
// model can call, indistinguishable from the built-in Bash/Read/Edit set.
//
// Slack's hosted MCP server (https://mcp.slack.com/mcp) requires a Slack user
// OAuth token — it does not support anonymous access or dynamic client
// registration. Getting that token is a one-time setup step outside this
// program:
//
//  1. Register a Slack app (https://api.slack.com/apps) and add the scopes
//     your chosen tools need — e.g. search:read.public/search:read.private
//     for the search tool, channels:read for channel listing. See
//     https://docs.slack.dev/ai/slack-mcp-server/ for the full scope list.
//  2. Run the OAuth flow once: send the user to
//     https://slack.com/oauth/v2_user/authorize with your app's client_id
//     and the scopes above, then exchange the returned code at
//     https://slack.com/api/oauth.v2.user.access for a user access token.
//  3. Export that token as SLACK_MCP_TOKEN and run this example.
//
// In production you'd persist and refresh that token rather than pasting it
// into an env var each run — see mark3labs/mcp-go's client/transport
// WithHTTPOAuth for a client that drives the full OAuth 2.0 flow itself.
//
//	export LLM_GATEWAY_BASE_URL=... LLM_GATEWAY_KEY=...
//	export SLACK_MCP_TOKEN=xoxp-...          # user token from step 2 above
//	go run ./examples/usage-with-mcp
package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	toroid "github.com/yashbonde/toroid-kernel"
	"github.com/yashbonde/toroid-kernel/tools"
)

func main() {
	ctx := context.Background()

	apiKey := os.Getenv("LLM_GATEWAY_KEY")
	model := "llmgateway/claude-haiku-4-5"
	if apiKey == "" {
		fmt.Println("set LLM_GATEWAY_KEY to run this example")
		return
	}
	if m := os.Getenv("TOROID_MODEL"); m != "" {
		model = m
	}

	slackToken := os.Getenv("SLACK_MCP_TOKEN")
	if slackToken == "" {
		fmt.Println("set SLACK_MCP_TOKEN to run this example — see the file header for how to obtain one")
		return
	}

	k, err := toroid.NewKernel(ctx, toroid.Config{
		Model:                model,
		APIKey:               apiKey,
		WorkDir:              ".",
		IncludeComputerTools: true, // keep the usual Bash/Read/Edit set alongside Slack's tools
		MCPServers: []tools.MCPServerConfig{
			{
				Name:    "slack",
				BaseURL: "https://mcp.slack.com/mcp",
				Headers: map[string]string{
					"Authorization": "Bearer " + slackToken,
				},
			},
		},
	})
	if err != nil {
		// A connect/initialize/list-tools failure against the Slack server
		// (bad token, missing scope, app not approved) surfaces here.
		panic(err)
	}
	defer k.Close()

	// Confirm what Slack exposed — every tool from this server is prefixed
	// "slack__" (MCPServerConfig.Name), so tools from other servers (or
	// toroid's own built-ins) can't collide with it.
	fmt.Println("== tools registered from Slack's MCP server ==")
	for name := range k.Tools.Tools() {
		if strings.HasPrefix(name, "slack__") {
			fmt.Println("-", name)
		}
	}

	fmt.Println("\n== run: read specific Slack message ==")
	out, usage, err := k.Run(ctx, "Read the Slack message in channel C0BFX1WLMGD with timestamp 1783526212.078379 (URL: https://foobartesting.slack.com/archives/C0BFX1WLMGD/p1783526212078379) and tell me exactly what the message says. Quote the full text of the message.")
	if err != nil {
		panic(err)
	}
	fmt.Println(out)
	fmt.Printf("sessions billed: %d | cost: $%.6f\n", len(usage.Tokens), k.RunningCostUSD())
}
