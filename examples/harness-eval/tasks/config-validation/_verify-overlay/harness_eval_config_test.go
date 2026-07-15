package toroid

import (
	"context"
	"strings"
	"testing"
)

func TestHarnessConfigValidationRejectsInvalidValues(t *testing.T) {
	noSkills := false
	cases := []struct {
		name  string
		cfg   Config
		field string
	}{
		{"thinking", Config{Thinking: Thinking("medium")}, "Thinking"},
		{"repeat", Config{MaxRepeatCalls: -1}, "MaxRepeatCalls"},
		{"context", Config{TotalContextSize: 100, CompactionBufferSize: 100}, "CompactionBufferSize"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.cfg.Model = "llmgateway/glm-5p2"
			tc.cfg.APIKey = "unused"
			tc.cfg.WorkDir = t.TempDir()
			tc.cfg.IncludeComputerTools = false
			tc.cfg.LoadSkills = &noSkills
			k, err := NewKernel(context.Background(), tc.cfg)
			if k != nil {
				k.Close()
			}
			if err == nil {
				t.Fatalf("expected %s validation error", tc.field)
			}
			if !strings.Contains(strings.ToLower(err.Error()), strings.ToLower(tc.field)) {
				t.Fatalf("error %q does not name %s", err, tc.field)
			}
		})
	}
}

func TestHarnessConfigValidationAllowsBoundary(t *testing.T) {
	noSkills := false
	k, err := NewKernel(context.Background(), Config{
		Model: "llmgateway/glm-5p2", APIKey: "unused", WorkDir: t.TempDir(),
		IncludeComputerTools: false, LoadSkills: &noSkills,
		Thinking: ThinkingLow, TotalContextSize: 101, CompactionBufferSize: 100,
	})
	if err != nil {
		t.Fatalf("valid boundary rejected: %v", err)
	}
	k.Close()
}
