# Toroid Chat (Android)

A tiny Android chat app whose brain is the toroid kernel. Replies stream token
by token, and reasoning models show their thinking as it arrives. The agent
can manage your to-do list, which it keeps in the phone's calendar, and save
documents you ask for where the Files app shows them. The model
picker asks for a class first (Flash, Mid, or Frontier), then lists the newest
models in it, all through [OpenRouter](https://openrouter.ai): open-weight
models from DeepSeek, Qwen, GLM, MiniMax, Kimi and others, plus Claude, GPT,
Gemini and Grok.

```text
WebView ──SSE──▶ Go server on 127.0.0.1 ──▶ toroid Kernel.Stream ──▶ OpenRouter
   ▲                  (inside the APK)
   └── Activity       Foreground Service: runs the server, holds a wake lock + Wi-Fi lock
```

- `main.go`, `server.go`, `stream.go`, `models.go`, `settings.go`: the Go server. One kernel
  per chat, a $0.50 spend cap per chat, and live prices from OpenRouter's
  model list.
- `todos.go`: the agent's to-do tools, which forward to the phone (below).
- `agents.go`, `images.go`, `palette.go`: agent setups, model ids starting
  with `@` that name a whole agent stored in SQLite, and the one built in, the
  image agent (see [Agent setups](#agent-setups)).
- `files.go`: the agent's file tools, which forward to the phone the same way.
- `history.go`: saved chats, one JSON file per chat in `files/chats/`.
- `turn.go`: a reply runs on its own, not on the request that asked for it, so
  it keeps going when the app goes to the background. Its events are kept: the
  UI follows it again from `/api/chats/{id}/live` when it comes back (or when
  the chat is opened from History), and Stop calls `/api/chats/{id}/stop`.
  While a reply runs, a line under it says who it is waiting on: the chat
  model deciding, the vision model looking at a screenshot, or a tool on the
  phone.
- `web/index.html`: the UI, embedded in the binary. The header shows only the
  model id and the chat's spend; the model picker and prices are under **⋯**.
  Amounts are shown in rupees at a flat ₹100 to the dollar (the server counts
  in USD). While the agent drives an app, a small live view floats in the
  bottom right; ✕ folds it into the header orb (a small screen with a dot
  circling it), and tapping the orb brings it back.
- `android/`: a foreground `Service` that runs the binary and the tool host
  (`ToolHost.java`, `Todos.java`, `Files.java`, `FilesProvider.java`), and an
  `Activity` that shows it in a WebView.
- `android/gen_icon.py`: draws the launcher icon, a pixel-art torus in the
  header orb's greens, and the notification icon, the orb at rest. The art is
  text in the script; edit it and rerun to rewrite `res/drawable/`.
- `build.sh`: builds the APK without Gradle.

## Requirements

- An arm64 phone on Android 10 (API 29) or later, with USB debugging on.
- Go, JDK 17, and the Android SDK with `platforms;android-29` and
  `build-tools;34.0.0`. The command-line tools are enough; Android Studio is
  not needed.
- An OpenRouter API key.

## Build and install

```bash
ANDROID_HOME=/path/to/android-sdk ./examples/android-chat/build.sh install
```

Set `JAVA_HOME` if JDK 17 is not at Homebrew's `openjdk@17` path. The first
build creates a debug keystore, `debug.jks`, next to the script (gitignored).
Keep it: Android only updates an app signed with the same key, and
reinstalling under a new key wipes the stored API key.

## Add your API key

The key is never part of the source, the APK, or a build setting, and the app
has no field to paste it into. Push it from a computer instead. The app is
debuggable, so `run-as` can write to its private storage:

```bash
read -rs KEY && printf %s "$KEY" | adb shell 'run-as com.toroid.chat sh -c "mkdir -p files && cat > files/openrouter.key && chmod 600 files/openrouter.key"'
```

The server reads `files/openrouter.key` (mode 600), or `LLM_GATEWAY_KEY` when
run on a computer. `/api/status` reports only whether a key exists, never its
value.

## Try it on a computer

```bash
mkdir -p /tmp/chat && read -rs KEY && printf %s "$KEY" > /tmp/chat/openrouter.key
go run ./examples/android-chat -data /tmp/chat
open "http://127.0.0.1:18765/?token=$(cat /tmp/chat/token)"
```

## To-dos

Ask in plain words: "add a todo to buy milk tomorrow", "what's on my list?",
"mark buy milk as done". The agent has five to-do tools, five file tools (see
[Files](#files)), the app tools (see [Apps](#apps)) and nothing else (no shell): `tasks_list`, `task_add`,
`task_update`, `task_done` and `task_delete`. Each call shows in the reply as
a small line, such as `✓ added: Buy milk · Sat 14 Nov`, and saved chats keep
those lines.

- **Where they live.** In a local calendar named "Toroid", created on first
  use, so they show in the Calendar app too. Each to-do is an all-day event on
  its due date (today if none is given). The event's title is the to-do, with
  a leading `✓ ` once done; its description starts with `status: open` or
  `status: done`, and the rest is notes. The tools only touch events in that
  calendar.
- **How the server reaches it.** Calendar access needs Android APIs, so the
  `Service` runs a small tool host on an abstract Unix socket,
  `@toroidchat-tools`: one JSON request per line, `{"op": "...", "args": {...}}`,
  answered by `{"ok": true, "result": ...}` or `{"ok": false, "error": "..."}`.
  Any app can connect to an abstract socket, so the host only answers peers
  with the app's own uid, which the Go server (its child process) has.
- **Permission.** The app asks for Calendar access on launch. Without it the
  tools answer with an error saying how to turn it on (Settings › Apps ›
  Toroid Chat › Permissions). For testing:
  `adb shell pm grant com.toroid.chat android.permission.WRITE_CALENDAR` (and
  `READ_CALENDAR`).

## Files

Ask to keep something: "save a doc with a packing list for the beach", "save
that as a markdown file too", "open it", "what files do I have?". The agent
saves only when you explicitly ask it to store or save something, and deletes
only when you ask.

- **Where they live.** In shared storage at `Documents/Toroid`, so the Files
  app shows them under Documents › Toroid. The folder is made on the first
  save.
- **Tools.** `files_list` (name, size, modified time and type, newest first),
  `file_save` (`name`, `content`, optional `format` and `overwrite`),
  `file_read` (text files, the first 64 KB, with a note when cut short; media
  is refused with a short description), `file_open` and `file_delete`. Each
  call shows as a line such as `✓ saved: Packing list.html (2 KB) ·
  Documents/Toroid` or `✓ opened: notes.md as text`.
- **Formats.** A "doc", "document", "note" or "file" with no format named is
  saved as a self-contained HTML page: a title, a small inline stylesheet that
  reads well on a narrow screen in light colours, headings, lists and tables,
  and nothing loaded from elsewhere. Name a format and it is used instead:
  `.txt` for plain text, or `.md`, `.json`, `.csv` and other text formats.
  `file_save` writes text only.
- **How each kind opens.** `file_open` starts a viewer on a read-only
  `content://com.toroid.chat.files/<name>` URI (`FilesProvider`, scoped to
  that one folder): HTML as `text/html` (HTML Viewer, the browser view for
  local files), images and video under their own type (Gallery), audio
  (Music), and any other text as `text/plain`. If no app handles the type, as
  for PDF on a phone without a PDF viewer, the tool says so. Android 10 lets
  an app start a viewer only while it is on screen, so `file_open` works while
  the chat is open and otherwise says where the file is.
- **Safety.** A name is one file name: letters, digits, spaces and `. _ ( ) -`,
  up to 80 characters plus an extension. No folders, no `..`, no hidden
  (dot) files. The Go tools check names before anything reaches the phone,
  and `Files.java` checks again and resolves the canonical path, which must
  sit directly in `Documents/Toroid`. A save is at most 1 MB and never
  replaces a file unless `overwrite` is set. All file I/O happens in Java; the
  Go server never touches shared storage.
- **Permission.** The app asks for Storage access on launch, and uses
  `requestLegacyExternalStorage` for plain file access on Android 10. After a
  save or delete it tells the media scanner, so Files and Gallery update at
  once. Without the permission the tools say how to turn it on. For testing:
  `adb shell pm grant com.toroid.chat android.permission.WRITE_EXTERNAL_STORAGE`
  (and `READ_EXTERNAL_STORAGE`), then force-stop and reopen the app so the
  grant applies.

## Apps

Ask the agent to use an app: "use the calendar app to add an event tomorrow".
It only ever drives apps on an off-screen virtual display, never your screen,
through the separate **Toroid System** bridge: a headless system-uid app that
owns the displays and listens on the abstract socket `@toroid-system`. Without
the bridge the app tools just say it is not reachable.

- **Tools.** `app_list`, `app_open` (makes the app active on a background
  display, created on first use; with `url` it opens a web page in the
  browser instead, so the agent never types into the address bar, where
  Jelly ignores an injected Enter), `screen_look` (what is on screen, read
  from a screenshot with a labelled coordinate grid; takes an optional
  `question`), `tap`,
  `swipe` (a drag that scrolls by its own length, without a fling), `type`,
  `key` (back, enter, del, tab; refused unless the bridge can confirm it
  reaches the app) and `app_close`.
- **A vision model looks, the chat model acts.** By default the chat model
  never sees screenshots. The kernel's vision delegate sends each one to
  DeepSeek V4.1 Flash (`deepseek/deepseek-v4.1-flash`, also the default chat
  model), with the agent's plan and its `question`, at a steady, low price.
  Any image-capable model can be picked instead;
  [Jev Router](https://openrouter.ai/typesafe/jev-router)
  (`typesafe/jev-router`) is offered too, but it took 7-13 s a look and its
  price varied from call to call. The vision model answers the question and lists every visible control and text with its
  centre and bounds in pixels, read off the grid. The chat model plans and taps
  from that text, so even a text-only Flash model can drive apps. Its cost
  is part of each reply's cost; the `done` event also reports it as
  `vision_cost_usd` over `vision_calls` looks.
- **Settings.** `GET /api/settings` returns `vision_enabled`, `vision_model`
  and suggested `vision_models`. `POST /api/settings` with
  `{"vision_enabled": false}` or `{"vision_model": "<openrouter id>"}`
  changes them (`""` restores DeepSeek V4.1 Flash), and a chat picks the change up on its next
  message. They are kept in `files/settings.json`. With the delegate off, a
  vision chat model gets the screenshot itself, and a text-only one gets only
  the text description. Whether a model takes images comes from OpenRouter's
  `architecture.input_modalities`.
- **Live view.** While a reply uses an app, a small floating window in the
  bottom right shows it at about 5 frames a second (`GET
  /api/display/{id}/frame.jpg`, token required). Tap it for a larger view. It
  closes when the reply ends; nothing is kept in the chat but the tool lines,
  such as `opened Calendar (in background)` or `tapped (412, 290)`, each with a
  small icon. Long runs fold into one row, such as `Calendar · 14 steps`.
- **Rules.** The agent prefers the to-do and file tools, looks before and
  after every tap, never types passwords or payment details, and asks before
  anything destructive. On the web it opens the site or a DuckDuckGo search
  by URL, only reads (never downloads, installs or sets anything unasked),
  stops once it has the answer, and reports only what it actually saw.
- **When a reply runs out of steps.** A reply has at most 20 steps. If it
  stops there (or the kernel's repeat guard stops it), the reply ends with a
  line saying so, and "continue" picks it up again.

## Agent setups

A model id that starts with `@` names an **agent setup** instead of a model:
a whole agent kept in SQLite (`files/agents.db`, table `agent_setups`) with
its own system prompt, tools, models and spend cap. Every other model id
works exactly as before, and the database is only opened the first time an
`@` id is used.

| Column | Meaning |
|---|---|
| `id` | the model id, e.g. `@image-gen-agent` |
| `system_prompt` | the agent's whole system prompt |
| `flash_model`, `mid_model`, `frontier_model` | the LLM per class; an empty Mid falls back to Flash, an empty Frontier to Mid |
| `image_draft_model`, `image_final_model` | OpenRouter image models for the image tools |
| `tools` | comma-separated tool names |
| `spend_cap_usd` | per-chat cap (0: the server's `-max-spend`) |
| `version` | built-in setups are rewritten at startup only by a newer version, so edits in the database survive restarts |

`@name` runs on the setup's Mid model; `@name:flash`, `@name:mid` and
`@name:frontier` pick a class. Send it to `POST /api/chat` like any model id;
the reply streams the same events, plus `image` (below) and an `activity` of
kind `image` while an image is being drawn.

### The image agent

One setup is built in, `@image-gen-agent`. It writes image prompts in a
`<scene>: … <design>: … <colours>: …` structure, aimed at bold, poster-like
phone wallpapers in vivid, richly detailed, high-resolution pixel art (fine
pixels and many colours, not chunky 8- or 16-bit blocks): a calm empty band about 18% from the top for the
clock (it never draws a clock or any text) and the subject inside the middle
80% of the width, since the phone crops 9:16 to 720×1480; tall subjects stop
below a third of the height. Its classes are GLM 5.3 Flash (Flash, also used
for palettes), Claude Sonnet 5.5 (Mid, the default) and Claude Opus 5.5
(Frontier). It has four tools:

- `image_generate(prompt, quality, model, shape)`: `POST /images` on
  OpenRouter with `aspect_ratio` set to `shape` (9:16, the default and a
  phone wallpaper; or 2:3, 1:1, 3:2). `model` is a model the user picked in
  the Gallery (one of those in `imagePriceGuess`); Recraft gets 3:4 or 4:3 in
  place of 2:3 or 3:2. Without it, `draft` (the default) uses FLUX 3 Image Klein
  (`black-forest-labs/flux-3-image-klein`, about $0.009 and 10 s an image,
  the cheapest well-known image-input model in OpenRouter's catalog); `final`
  uses GPT Image 2.5 Sunburst (`openai/gpt-image-2.5-sunburst`, about $0.035
  and 30 s). A parameter a model refuses (FLUX does not take `quality`) is
  dropped and the call retried. The Gallery's pickers send content, style,
  colours, shape and model as an `[Options]` block at the end of the user's
  message; the system prompt tells the agent to use each part nearly
  verbatim and pass shape and model to this tool.
- `image_recolour(image, palette_description, quality)`: the same call with
  the image as an `input_references` data URL, asking to keep the composition
  and change only the colours. The new image records the old one as its parent.
- `palette_pick(image)`: counts the image's 12 most common colours in Go
  (5-bit bins merged when closer than 28 in RGB, each with its share of the
  image), then asks the Flash model to assign the roles `bg, surface, line,
  text, muted, accent, accent_hi, warn, bad, tint1, tint2, tint3` twice: a
  dark mode and a light mode (asked again with the reason, up to three tries,
  while either is incomplete). Code then holds each mode to its rules
  (`settle` in `palette.go`), changing as little as it can: dark mode on a
  near-black, nearly grey background (a faint hint of the image's hue at most,
  never navy or teal) with near-white text; light mode on near-white with
  near-black text; text at 4.5:1 and muted text at 4:1 on bg; accents and
  tints at 4.5:1 on surface (apps use them as text too); and warn and bad on hues clearly apart from the
  accent (else a plain amber/yellow/orange and red/crimson/pink-red, whichever
  stands furthest from it). Both modes are stored with the image; a palette
  stored before light mode existed is read again.
- `image_look(image)`: loads the image (an id, or the path of a picture the
  user attached from the Gallery with `[Image] <path>` at the end of a
  message) and returns it as a tool-result image. The kernel's vision
  delegate, the vision model from Settings, describes it; the LLM reads only
  that description. Without a vision model the image is dropped.

Images are saved to shared storage at `Pictures/Toroid/` (named
`toroid-YYYYMMDD-HHMMSS-xxxx.jpg|png`) and handed to the media scanner, so
gallery apps show them at once, and recorded in `agents.db` (table `images`:
prompt, model, quality, cost, parent image, chat and setup). As with the
other tools the Go server does no shared-storage I/O: it writes the image to
its private `files/images/`, and the tool host (`Images.java`, ops
`image_publish` and `image_load`) copies it out, or reads a shared image into a
small private PNG for the server. Image and palette costs are OpenRouter's
`usage.cost` and count toward the chat's spend cap; an image the remaining
budget cannot cover is refused before it is drawn. The setup's cap is $0.50
a chat.

Two endpoints serve other apps (token required):

- `POST /api/palette` `{"image": "<path or image id>", "refresh": false}`:
  the image's palette, the stored one at no cost unless `refresh`.
- `GET /api/image?ref=<path, file name or id>`: what `agents.db` knows about
  an image (prompt, model, cost, parent, palette), or 404.

### Using the server from another app

The token is kept in `files/token` and reused across restarts (a missing or
malformed file gets a fresh one), so another app can be given a copy once.
For a debuggable app with the package `com.example.app`:

```bash
adb exec-out run-as com.toroid.chat cat files/token | adb shell 'run-as com.example.app sh -c "mkdir -p files && chmod 700 files && cat > files/toroid.token && chmod 600 files/toroid.token"'
```

The server runs in Toroid Chat's foreground service, which keeps running
when the chat screen is closed or swiped away. It is not running after a
reboot or an update until Toroid Chat is opened once, nor after **Stop** or
**Quit app**.

## Notes

- **Security.** The server listens on 127.0.0.1 only. Other apps on the phone
  can reach loopback too, so every `/api/*` call needs a random
  token, and requests with a non-loopback `Host` are refused.
- **Battery.** While the service runs it holds a partial wake lock and a Wi-Fi
  lock, so replies keep working with the screen off. Use **Stop** on the
  notification or **Quit app** in settings to release them.
- **History.** Every chat is saved after each reply. The calendar button opens
  a month view whose circles get greener with the number of chats that day,
  over a list of every day's chats; scrolling the list moves the calendar.
  Tap a chat to reopen it and keep going, even after a restart: the server
  rebuilds the conversation from the saved file. Long-press a chat to delete it.
- **Cost.** Each reply's cost is OpenRouter's billed `usage.cost`, which the
  kernel records per step. The cap is per chat, and switching models keeps both
  the history and the money already spent.
- **Themes.** The UI follows a phone-wide theme named in the global setting
  `toroid_ui_theme`: `phosphor` (the default: green accent, mono tool lines),
  `garden` (warmer surfaces, tool lines tinted by kind: apps blue, files amber,
  to-dos rose), `crt` (green on near-black in mono, tool lines as amber `>`
  prompts, an amber block cursor while a reply streams, and an amber-lit orb)
  or `custom`, built from a palette in the global setting `toroid_ui_palette`:
  a JSON object from each role (`bg`, `surface`, `line`, `text`, `muted`,
  `accent`, `accent_hi`, `warn`, `bad`, `tint1`, `tint2`, `tint3`) to
  `#RRGGBB`. In custom the page sets its CSS variables from the roles (surface
  for panels, `bad` for errors, `accent_hi` for the streaming caret and the
  orb's sparks, the tints for tool lines by kind: apps, files, to-dos) and
  mixes the rest; with a missing or broken palette it is phosphor.
  The app only reads the settings, through `ToroidApp.theme()` and
  `ToroidApp.palette()`, and a `ContentObserver` on both calls
  `window.applyTheme(name)` when either changes, so a switch, or a new
  palette while custom is on, applies at once. Another app, such as a launcher, writes it; to try one by
  hand: `adb shell settings put global toroid_ui_theme crt`. In a desktop
  browser there is no `ToroidApp` and the page stays phosphor.
