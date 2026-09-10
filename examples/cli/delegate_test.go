package main

import "testing"

func TestRouteDelegationMatchesWholeWords(t *testing.T) {
	tests := []struct {
		prompt string
		want   DelegationTarget
	}{
		// Substring matching used to send these to the wrong harness: "doc"
		// inside docker/documentation, "hr" inside thread/threshold.
		{"fix the docker build", TargetTRK},
		{"walk the thread and fix the deadlock", TargetTRK},
		{"raise the threshold in the rate limiter", TargetTRK},
		{"add docstrings to the parser", TargetTRK},
		{"implement retry logic", TargetTRK},

		{"summarize the doc and post to slack", TargetSlack},
		{"draft an email to finance", TargetSlack},

		{"audit the auth layer", TargetCodex},
		{"evaluate this large codebase", TargetCodex},
		{"review the migration plan", TargetCodex},
	}
	for _, tt := range tests {
		if got := routeDelegation(tt.prompt); got != tt.want {
			t.Errorf("routeDelegation(%q) = %q, want %q", tt.prompt, got, tt.want)
		}
	}
}

func TestParseDelegateArgsExtractsModel(t *testing.T) {
	tests := []struct {
		in         string
		wantPrompt string
		wantModel  string
	}{
		{"fix the parser", "fix the parser", ""},
		{"--model gpt-5.6-sol fix the parser", "fix the parser", "gpt-5.6-sol"},
		{"-m deepseek-v4-pro fix the parser", "fix the parser", "deepseek-v4-pro"},
		// A bare flag with no value is task text, not a model.
		{"--model", "--model", ""},
		// Only leading flags count; a later --model belongs to the task.
		{"run tests --model foo", "run tests --model foo", ""},
	}
	for _, tt := range tests {
		prompt, model := parseDelegateArgs(tt.in)
		if prompt != tt.wantPrompt || model != tt.wantModel {
			t.Errorf("parseDelegateArgs(%q) = (%q, %q), want (%q, %q)", tt.in, prompt, model, tt.wantPrompt, tt.wantModel)
		}
	}
}

func TestNewTaskIDIsUniqueWithinASecond(t *testing.T) {
	seen := map[string]bool{}
	for range 100 {
		id := newTaskID()
		if seen[id] {
			t.Fatalf("duplicate task id %q — records would overwrite each other", id)
		}
		seen[id] = true
	}
}
