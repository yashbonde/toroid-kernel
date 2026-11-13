package main

import (
	"os"
	"testing"

	toroid "github.com/yashbonde/toroid-kernel"
)

// TestMain points the gateway at an address where nothing listens, so no test
// reaches a real LLM gateway (for example one set in the developer's shell)
// when it builds a kernel: the model-limits lookup fails fast and offline.
// Tests that need a gateway start their own httptest server.
func TestMain(m *testing.M) {
	os.Setenv(toroid.GatewayBaseURLEnv, "http://127.0.0.1:1")
	os.Unsetenv(toroid.GatewayKeyEnv)
	os.Exit(m.Run())
}
