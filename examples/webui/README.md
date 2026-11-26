# Toroid Web UI

From the repository root, start the server and open the private loopback link it prints.

```sh
go run ./examples/webui
```

The default port is 8787. Use `-port 0` to pick an available port. The private link pairs the browser with this workspace and port for 30 days, including across server restarts. When opening the plain address in a new browser, paste the printed link into **Connect this browser**. The frontend is embedded in the Go binary and runs without a Node build.

## Chat

The sidebar shows conversations started in the web UI, including agent-builder tests. It reads from `~/.toroid/sql.db` and honors `TOROID_HOME`. It uses the last 15 days of root activity. Pinned chats stay visible until unpinned. Search filters sidebar titles.

The split button beside **New chat** lets you choose an existing conversation or start a new one in a side-by-side pane. The split icon in each pane header lets you change its conversation. Up to three panes can stream at once. Click a pane to select where sidebar actions open. Use **Arrange conversations** to group by date, workspace, or agent. Date groups use local time and show the most recent activity first. Today and the preceding six days include the weekday. The **Settings** button controls appearance and reading width across panes.

Choose a model from the searchable provider catalog in the composer. Select a workspace folder before the first message to set the kernel working directory for the lifetime of the chat. Chats without an explicit workspace use the server’s working directory and appear under **Conversations** when grouped by workspace.

Use the paperclip or drop files and folders into a chat. The message includes their local paths. When a browser hides a dropped item's location, choose its containing folder to confirm the path. PNG, JPEG, GIF, and WebP images up to 5 MiB appear as previews and are sent as image input with an image-capable model. Reopened chats and forks load previews from the original paths.

Use the pencil beside a chat title to rename it. **Fork** below a message or inside a tool call opens an independent chat with the history through that point, using the same agent, model, and workspace.

Consecutive tool calls appear in a collapsed group. Expand it and select a call to inspect its arguments and output.

Replies render Markdown as text arrives. **Aa** opens formatting for text, links, lists, quotes, and code. Enter inserts a new line. Shift+Enter and Cmd+Enter (Ctrl+Enter elsewhere) send the message. At the end of a trailing code block, Right or Down moves into a new paragraph. At the bottom, the transcript follows the reply. Scroll up to read, or select a passage to open its comment input. Enter places the comment in the right margin beside the selected passage. Click the quote on a card to return to its source. Pending comments also appear inside the composer, where you can edit the note or remove it before sending. This browser saves pending comments per chat across refreshes. The composer includes each quote and comment once, in the next accepted message. Sent comments stay in the margin as read-only notes marked **Resolved**. Use the speech bubble in the chat header to hide or show the margin and passage highlights. Each chat remembers its visibility setting in this browser. **Latest** returns to the live end of the conversation. The square button stops the current reply.

The address bar follows the active chat, including when switching panes. Back and Forward revisit chat selections. Reloading restores the pane layout and reconnects to running replies. Switching chats, closing a pane, or leaving the page lets generation continue on the local server. A pulsing green dot marks a finished reply until you view its chat.

The composer footer shows total spend for completed calls, including delegates and compaction, at the CLI conversion of ₹100 per USD. Hover over the amount to see whether it was estimated from recorded token usage and gateway catalog rates. Historical estimates use current rates. Missing prices display as a dash. Forks start their own spend totals. The context bar shows the latest token usage against the model input limit when available. Hover to see exact counts and the recorded kernel window.

## Agents

**base** uses the CLI's default model (`deepseek/deepseek-v4p1-flash`), `TOROID_MODEL` override, provider credentials, and the directory where the server was launched. The kernel reads `LLM_GATEWAY_BASE_URL` and `LLM_GATEWAY_KEY` for gateway models, or `OPENAI_API_KEY` for OpenAI models. The explicit `TOROID_LLM_TOKEN` override is scoped to the base provider route.

Open **Agent builder** to configure instructions, thinking level, computer tools, local skills, and delegation. Instructions extend the built-in system prompt. Send a test message on the right, revise settings, and test again in the same conversation. **Save agent** adds the named configuration to the composer picker. Switching agents applies the new configuration to the next turn and keeps the chat history. Choose the draft model in the test chat composer. Saved agents can be deleted in the builder. Their conversations continue with **base**. Test conversations also appear in the sidebar.

Computer tools operate in the chat’s working directory. Web turns support foreground shell work and synchronous subagents and stop when you press the square button. The web UI stores pins, agent configurations, draft settings, and conversation origins in `webui_*` tables. Toroid's event log holds the messages.

## Rebuilding the editor

The repository includes the browser bundle for ProseMirror and Markdown-it. After editing the composer source or updating dependencies, rebuild it from the repository root.

```sh
npm --prefix examples/webui/frontend ci
npm --prefix examples/webui/frontend run build
```
