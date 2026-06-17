package tools

import (
	"context"
	"fmt"
	"os/exec"
	"runtime"

	"charm.land/fantasy"
)

type NotifyArgs struct {
	Title   string `json:"title" jsonschema:"description=Notification title"`
	Message string `json:"message" jsonschema:"description=Notification message"`
}

// NotifySink delivers a notification somewhere (desktop, webhook, Slack, or a
// peer kernel). Sinks are registered with RegisterNotifySink and run in addition
// to the EventNotification fired on the kernel's event bus, so a host can route
// notifications without the tool hardcoding any one platform. §12.3.
type NotifySink func(ctx context.Context, title, message string) error

var notifySinks []NotifySink

// RegisterNotifySink adds a delivery sink. Safe to call before constructing the
// kernel. Errors from sinks are best-effort and never fail the tool call.
func RegisterNotifySink(s NotifySink) { notifySinks = append(notifySinks, s) }

// desktopNotify is the default best-effort, platform-agnostic local notifier.
// It never returns an error that would fail the tool — on headless/CI hosts or
// unsupported platforms it simply no-ops.
func desktopNotify(ctx context.Context, title, message string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		script := fmt.Sprintf(`display notification %q with title %q`, message, title)
		cmd = exec.CommandContext(ctx, "osascript", "-e", script)
	case "linux":
		if _, err := exec.LookPath("notify-send"); err != nil {
			return
		}
		cmd = exec.CommandContext(ctx, "notify-send", title, message)
	case "windows":
		ps := fmt.Sprintf(`[void][System.Reflection.Assembly]::LoadWithPartialName('System.Windows.Forms');`+
			`$n=New-Object System.Windows.Forms.NotifyIcon;$n.Icon=[System.Drawing.SystemIcons]::Information;`+
			`$n.Visible=$true;$n.ShowBalloonTip(5000,%q,%q,[System.Windows.Forms.ToolTipIcon]::Info)`, title, message)
		cmd = exec.CommandContext(ctx, "powershell", "-NoProfile", "-Command", ps)
	default:
		return
	}
	_ = cmd.Run() // best-effort
}

func NewNotifyTool(a Agent, desc string) *ToolDef {
	fTool := fantasy.NewAgentTool("notify", desc, func(ctx context.Context, args NotifyArgs, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
		title := args.Title
		message := args.Message

		// Primary: emit on the event bus so any host observer/sink can react.
		_ = a.Fire(ctx, "Notification", map[string]any{
			"title":   title,
			"message": message,
		})

		// Registered sinks (webhook, Slack, peer kernel, …) — best-effort.
		for _, s := range notifySinks {
			_ = s(ctx, title, message)
		}

		// Default local desktop notification — best-effort, never fatal.
		desktopNotify(ctx, title, message)

		return fantasy.ToolResponse{Type: "text", Content: "ok"}, nil
	})

	return &ToolDef{
		Name:        "notify",
		Description: desc,
		Template:    "notify.tool.tmpl",
		AgentTool:   fTool,
	}
}
