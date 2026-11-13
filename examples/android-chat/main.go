// Pattern: phone chat — a tiny Android app whose brain is this kernel.
//
// The Go side is a loopback-only HTTP server. It serves an embedded single-page
// UI and drives one Kernel per chat over Server-Sent Events, so a WebView (or
// curl) sees the answer token by token. The Android side (android/) is a
// foreground Service that runs this binary as a child process while holding a
// wake lock and a Wi-Fi lock, plus an Activity that is just a WebView on
// http://127.0.0.1:<port>. build.sh packages the binary into the APK as
// lib/arm64-v8a/libtoroidchat.so so it lands, executable, in nativeLibraryDir.
//
// Models are reached through OpenRouter acting as the kernel's gateway: every
// id is "llmgateway/<openrouter id>", the kernel strips the prefix, and the
// request goes to LLM_GATEWAY_BASE_URL (defaulted here to OpenRouter). The key
// is read from <data>/openrouter.key (or $LLM_GATEWAY_KEY) and can be replaced
// from the UI.
//
// Run it on a laptop to try the UI without a phone:
//
//	echo -n sk-or-... > /tmp/chat/openrouter.key
//	go run ./examples/android-chat -data /tmp/chat
//	open http://127.0.0.1:18765/?token=$(cat /tmp/chat/token)
//
// Endpoints (all /api/* need the session token as the toroid_token cookie or
// an "Authorization: Bearer <token>" header; the token is written to
// <data>/token):
//
//	GET  /                 the chat UI
//	GET  /healthz          liveness probe, no auth
//	GET  /api/models       curated models with per-MTok prices
//	GET  /api/status       whether a key is configured (never the key itself)
//	POST /api/key          {"key": "..."} replace the stored key
//	POST /api/new          {"model": "..."} start a fresh chat, returns its id
//	POST /api/chat         {"session","model","message"} -> text/event-stream
//	GET  /api/history      every saved chat's summary, newest first
//	GET  /api/chats/{id}   one saved chat with its messages
//	GET  /api/chats/{id}/live   follow the reply running in a chat (replayed from its start); 204 if none
//	POST /api/chats/{id}/stop   end the reply running in a chat
//	DELETE /api/chats/{id} delete a saved chat
//	POST /api/palette      {"image": "<path or id>", "refresh": false} an image's UI palette (agents.go, images.go)
//	GET  /api/image?ref=   what agents.db knows about an image
//
// A model id starting with "@" names an agent setup stored in <data>/agents.db
// (see agents.go); every other id is an OpenRouter model as above.
//
// Chats are saved as <data>/chats/<id>.json after every reply; sending to the
// id of a saved chat that is not in memory resumes it from that file.
package main

import (
	"context"
	"crypto/rand"
	"embed"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	toroid "github.com/yashbonde/toroid-kernel"
)

//go:embed web
var webFS embed.FS

// openRouterBaseURL is the gateway used when LLM_GATEWAY_BASE_URL is unset.
const openRouterBaseURL = "https://openrouter.ai/api/v1"

func main() {
	addr := flag.String("addr", "127.0.0.1:18765", "listen address; must be a loopback address")
	data := flag.String("data", "", "writable data dir for the key file, token and kernel state (default ~/.toroid-chat)")
	watchStdin := flag.Bool("watch-stdin", false, "exit when stdin reaches EOF, so the server dies with the process that spawned it")
	spendCap := flag.Float64("max-spend", 0.50, "maximum LLM spend in USD per chat")
	flag.Parse()

	dataDir, err := resolveDataDir(*data)
	if err != nil {
		log.Fatalf("data dir: %v", err)
	}
	if err := requireLoopback(*addr); err != nil {
		log.Fatal(err)
	}

	// The kernel keeps its transcripts and default work dirs under TOROID_HOME
	// and $HOME; on Android neither exists for an app, so the service sets them.
	// Fill in anything still missing so a laptop run is self-contained too.
	if os.Getenv("TOROID_HOME") == "" {
		os.Setenv("TOROID_HOME", filepath.Join(dataDir, "toroid"))
	}
	if os.Getenv(toroid.GatewayBaseURLEnv) == "" {
		os.Setenv(toroid.GatewayBaseURLEnv, openRouterBaseURL)
	}

	token := os.Getenv("TOROID_CHAT_TOKEN")
	if token == "" {
		token = randomToken()
	}
	// Written so on-device tooling (adb run-as) can authenticate; mode 600 keeps
	// other apps out.
	if err := os.WriteFile(filepath.Join(dataDir, "token"), []byte(token), 0o600); err != nil {
		log.Fatalf("write token: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if *watchStdin {
		// The Android service holds our stdin pipe. If the app process dies
		// without destroying us, the pipe closes and we exit instead of holding
		// the port (and the network) as an orphan.
		go func() {
			_, _ = io.Copy(io.Discard, os.Stdin)
			log.Printf("stdin closed; shutting down")
			stop()
		}()
	}

	srv := newServer(dataDir, token, *spendCap)
	go func() {
		// Best-effort: replace the compiled-in prices with the live catalog's and
		// drop any model OpenRouter no longer lists.
		rctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		if err := srv.catalog.refresh(rctx, http.DefaultClient, os.Getenv(toroid.GatewayBaseURLEnv)); err != nil {
			log.Printf("model catalog refresh: %v (using built-in prices)", err)
		} else {
			log.Printf("model catalog refreshed: %d models", len(srv.catalog.list()))
		}
	}()

	hs := &http.Server{
		Addr:              *addr,
		Handler:           srv.routes(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		log.Fatalf("listen %s: %v", *addr, err)
	}
	log.Printf("toroid chat listening on http://%s (data %s)", ln.Addr(), dataDir)

	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = hs.Shutdown(sctx)
	}()
	if err := hs.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("serve: %v", err)
	}
	srv.sessions.closeAll()
}

// resolveDataDir returns an absolute, existing data directory.
func resolveDataDir(dir string) (string, error) {
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		dir = filepath.Join(home, ".toroid-chat")
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(abs, 0o700); err != nil {
		return "", err
	}
	return abs, nil
}

// requireLoopback refuses to listen anywhere but loopback: the server spends
// the user's OpenRouter credit on request.
func requireLoopback(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("addr %q: %w", addr, err)
	}
	if host == "localhost" {
		return nil
	}
	if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("addr %q: refusing to listen on a non-loopback address", addr)
	}
	return nil
}

func randomToken() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}
