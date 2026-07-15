package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
)

type traceRequestIDKey struct{}

type traceResponseBody struct {
	io.ReadCloser
	dst *os.File
}

func (b *traceResponseBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if n > 0 {
		_, _ = b.dst.Write(p[:n])
	}
	return n, err
}

func (b *traceResponseBody) Close() error {
	_ = b.dst.Close()
	return b.ReadCloser.Close()
}

type traceMeta struct {
	At      time.Time   `json:"at"`
	Method  string      `json:"method,omitempty"`
	URL     string      `json:"url,omitempty"`
	Status  int         `json:"status,omitempty"`
	Headers http.Header `json:"headers,omitempty"`
}

// startTraceProxy starts a loopback reverse proxy which records exact wire
// bodies and credential-redacted metadata. Trace files are mode 0600 because
// request bodies can contain source code and conversation history.
func startTraceProxy(targetRaw, outDir, listenAddr string) (string, func(), error) {
	target, err := url.Parse(targetRaw)
	if err != nil || target.Scheme == "" || target.Host == "" {
		return "", nil, fmt.Errorf("invalid proxy target %q", targetRaw)
	}
	if err := os.MkdirAll(outDir, 0o700); err != nil {
		return "", nil, err
	}

	var seq atomic.Uint64
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.FlushInterval = -1
	originalDirector := proxy.Director
	proxy.Director = func(req *http.Request) {
		originalDirector(req)
		req.Host = target.Host
	}
	proxy.ModifyResponse = func(resp *http.Response) error {
		id, _ := resp.Request.Context().Value(traceRequestIDKey{}).(uint64)
		prefix := filepath.Join(outDir, fmt.Sprintf("%04d", id))
		_ = writeTraceJSON(prefix+"-response-meta.json", traceMeta{
			At: time.Now().UTC(), Status: resp.StatusCode, Headers: redactHeaders(resp.Header),
		})
		f, err := os.OpenFile(prefix+"-response.body", os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
		if err == nil {
			resp.Body = &traceResponseBody{ReadCloser: resp.Body, dst: f}
		}
		return nil
	}
	proxy.ErrorHandler = func(w http.ResponseWriter, req *http.Request, err error) {
		id, _ := req.Context().Value(traceRequestIDKey{}).(uint64)
		_ = os.WriteFile(filepath.Join(outDir, fmt.Sprintf("%04d-error.txt", id)), []byte(err.Error()+"\n"), 0o600)
		http.Error(w, "upstream proxy error", http.StatusBadGateway)
	}

	handler := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		id := seq.Add(1)
		body, err := io.ReadAll(req.Body)
		if err != nil {
			http.Error(w, "read request", http.StatusBadRequest)
			return
		}
		_ = req.Body.Close()
		req.Body = io.NopCloser(strings.NewReader(string(body)))
		prefix := filepath.Join(outDir, fmt.Sprintf("%04d", id))
		_ = os.WriteFile(prefix+"-request.body", body, 0o600)
		_ = writeTraceJSON(prefix+"-request-meta.json", traceMeta{
			At: time.Now().UTC(), Method: req.Method, URL: req.URL.String(), Headers: redactHeaders(req.Header),
		})
		proxy.ServeHTTP(w, req.WithContext(context.WithValue(req.Context(), traceRequestIDKey{}, id)))
	})

	ln, err := net.Listen("tcp", listenAddr)
	if err != nil {
		return "", nil, err
	}
	server := &http.Server{Handler: handler, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = server.Serve(ln) }()
	stop := func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(ctx)
	}
	return "http://" + ln.Addr().String(), stop, nil
}

func redactHeaders(src http.Header) http.Header {
	dst := make(http.Header, len(src))
	for name, values := range src {
		lower := strings.ToLower(name)
		if strings.Contains(lower, "authorization") || strings.Contains(lower, "api-key") ||
			strings.Contains(lower, "token") || strings.Contains(lower, "cookie") {
			dst[name] = []string{"[REDACTED]"}
			continue
		}
		dst[name] = append([]string(nil), values...)
	}
	return dst
}

func writeTraceJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o600)
}

func envTrue(name string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(name))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

func setEnv(env []string, name, value string) []string {
	prefix := name + "="
	out := make([]string, 0, len(env)+1)
	for _, item := range env {
		if !strings.HasPrefix(item, prefix) {
			out = append(out, item)
		}
	}
	return append(out, prefix+value)
}

func runTraceProxyCommand(args []string) {
	if len(args) < 2 || len(args) > 3 {
		fatal("usage: harness-eval proxy <target-base-url> <trace-dir> [listen-address]")
	}
	listenAddr := "127.0.0.1:8787"
	if len(args) == 3 {
		listenAddr = args[2]
	}
	proxyURL, stop, err := startTraceProxy(args[0], args[1], listenAddr)
	must(err)
	defer stop()
	fmt.Println(proxyURL)
	fmt.Fprintf(os.Stderr, "recording requests in %s; press Ctrl-C to stop\n", args[1])
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	<-sig
}
