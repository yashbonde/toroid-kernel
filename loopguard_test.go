package toroid

import (
	"testing"

	"charm.land/fantasy"
)

// step builds a StepResult with a single tool call and its result.
func step(name, input, result string) fantasy.StepResult {
	return fantasy.StepResult{
		Response: fantasy.Response{
			Content: fantasy.ResponseContent{
				fantasy.ToolCallContent{ToolCallID: "c1", ToolName: name, Input: input},
			},
		},
		Messages: []fantasy.Message{
			{
				Role: fantasy.MessageRoleTool,
				Content: []fantasy.MessagePart{
					fantasy.ToolResultPart{ToolCallID: "c1", Output: fantasy.ToolResultOutputContentText{Text: result}},
				},
			},
		},
	}
}

func TestRepeatCallGuard(t *testing.T) {
	guard := (&Kernel{}).repeatCallGuard(3)

	tests := []struct {
		name  string
		steps []fantasy.StepResult
		stop  bool
	}{
		{
			name:  "three identical calls and results trips",
			steps: []fantasy.StepResult{step("read", `{"f":"a"}`, "x"), step("read", `{"f":"a"}`, "x"), step("read", `{"f":"a"}`, "x")},
			stop:  true,
		},
		{
			name:  "two identical does not trip (n=3)",
			steps: []fantasy.StepResult{step("read", `{"f":"a"}`, "x"), step("read", `{"f":"a"}`, "x")},
			stop:  false,
		},
		{
			name:  "same args but changing results is progress (polling)",
			steps: []fantasy.StepResult{step("poll", `{}`, "pending"), step("poll", `{}`, "pending"), step("poll", `{}`, "done")},
			stop:  false,
		},
		{
			name:  "different args does not trip",
			steps: []fantasy.StepResult{step("read", `{"f":"a"}`, "x"), step("read", `{"f":"b"}`, "x"), step("read", `{"f":"c"}`, "x")},
			stop:  false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := guard(tt.steps); got != tt.stop {
				t.Errorf("guard = %v, want %v", got, tt.stop)
			}
		})
	}
}

func TestRepeatCallGuardDisabled(t *testing.T) {
	guard := (&Kernel{}).repeatCallGuard(0)
	steps := []fantasy.StepResult{step("read", "{}", "x"), step("read", "{}", "x"), step("read", "{}", "x")}
	if guard(steps) {
		t.Error("guard with n=0 should never stop")
	}
}
