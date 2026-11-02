package llm

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"sync/atomic"
	"testing"
	"time"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

// A client timeout used to restart for every retry. A dead connection could
// therefore keep the CLI busy for four full timeout periods plus backoff.
func TestCompleteTimeoutCoversAllRetries(t *testing.T) {
	tests := []struct {
		name string
		chat func(*http.Client) Chat
	}{
		{
			name: "openai-compatible",
			chat: func(httpClient *http.Client) Chat {
				return &Client{BaseURL: "http://llm.test/v1", HTTP: httpClient}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var attempts atomic.Int32
			httpClient := &http.Client{
				Timeout: 40 * time.Millisecond,
				Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
					attempts.Add(1)
					<-req.Context().Done()
					return nil, req.Context().Err()
				}),
			}

			started := time.Now()
			_, err := tt.chat(httpClient).Complete(context.Background(), Request{Model: "test-model"})
			elapsed := time.Since(started)

			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("Complete error = %v, want context deadline exceeded", err)
			}
			if got := attempts.Load(); got != 1 {
				t.Fatalf("attempts = %d, want 1; the call deadline must stop retries", got)
			}
			if elapsed > 500*time.Millisecond {
				t.Fatalf("Complete took %s, want one shared 40ms deadline", elapsed)
			}
		})
	}
}

type unexpectedEOFBody struct{}

func (unexpectedEOFBody) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }
func (unexpectedEOFBody) Close() error             { return nil }

// A response-body disconnect must fail only that call. The shared HTTP client
// remains usable by the next CLI turn.
func TestCompleteRecoversAfterResponseBodyDisconnect(t *testing.T) {
	var calls atomic.Int32
	httpClient := &http.Client{
		Timeout: time.Second,
		Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			if calls.Add(1) == 1 {
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     make(http.Header),
					Body:       unexpectedEOFBody{},
				}, nil
			}
			body := `{"choices":[{"message":{"role":"assistant","content":"recovered"},"finish_reason":"stop"}],"usage":{}}`
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     make(http.Header),
				Body:       io.NopCloser(bytes.NewBufferString(body)),
			}, nil
		}),
	}
	c := &Client{BaseURL: "http://llm.test/v1", HTTP: httpClient}

	if _, err := c.Complete(context.Background(), Request{Model: "test-model"}); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("first Complete error = %v, want unexpected EOF", err)
	}
	resp, err := c.Complete(context.Background(), Request{Model: "test-model"})
	if err != nil {
		t.Fatalf("second Complete: %v", err)
	}
	if got := resp.Text(); got != "recovered" {
		t.Fatalf("second Complete text = %q, want recovered", got)
	}
}

func TestConstructorsUseBoundedHTTPClients(t *testing.T) {
	clients := []*http.Client{
		NewClient("http://llm.test/v1", "").HTTP,
	}
	for i, client := range clients {
		if client.Timeout != ClientTimeout {
			t.Errorf("client %d timeout = %s, want %s", i, client.Timeout, ClientTimeout)
		}
		if _, ok := client.Transport.(*http.Transport); !ok {
			t.Errorf("client %d transport = %T, want *http.Transport", i, client.Transport)
		}
	}
}
