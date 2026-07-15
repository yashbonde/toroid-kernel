package toroid

import (
	"encoding/json"
	"testing"

	"github.com/yashbonde/toroid-kernel/llm"
)

func TestNormalizeToolCallPathsRemovesExactWorkDir(t *testing.T) {
	workDir := "/tmp/harness work/clone"
	for _, command := range []string{
		`cd "/tmp/harness work/clone" && git status --short`,
		`cd '/tmp/harness work/clone' && go test ./...`,
	} {
		args, _ := json.Marshal(map[string]string{"command": command})
		parts := []llm.Part{llm.ToolCallPart{ID: "1", Name: "bash", Arguments: string(args)}}
		normalizeToolCallPaths(parts, workDir)
		call := parts[0].(llm.ToolCallPart)
		var got map[string]string
		if err := json.Unmarshal([]byte(call.Arguments), &got); err != nil {
			t.Fatal(err)
		}
		if got["command"] == command || got["command"] == "" {
			t.Fatalf("command was not normalized: %q", got["command"])
		}
	}
}

func TestNormalizeToolCallPathsPreservesDifferentDirectoryAndTools(t *testing.T) {
	bashArgs := `{"command":"cd /tmp/other && git status"}`
	readArgs := `{"filePath":"/tmp/work/file.go"}`
	parts := []llm.Part{
		llm.ToolCallPart{ID: "1", Name: "bash", Arguments: bashArgs},
		llm.ToolCallPart{ID: "2", Name: "read", Arguments: readArgs},
	}
	normalizeToolCallPaths(parts, "/tmp/work")
	if got := parts[0].(llm.ToolCallPart).Arguments; got != bashArgs {
		t.Fatalf("different-directory command changed: %s", got)
	}
	if got := parts[1].(llm.ToolCallPart).Arguments; got != readArgs {
		t.Fatalf("non-bash call changed: %s", got)
	}
}

func TestToolCallsMutateFiles(t *testing.T) {
	if toolCallsMutateFiles([]llm.ToolCallPart{{Name: "read"}, {Name: "bash"}}) {
		t.Fatal("inspection calls classified as file mutations")
	}
	if !toolCallsMutateFiles([]llm.ToolCallPart{{Name: "read"}, {Name: "edit"}}) {
		t.Fatal("edit call was not classified as a file mutation")
	}
}
