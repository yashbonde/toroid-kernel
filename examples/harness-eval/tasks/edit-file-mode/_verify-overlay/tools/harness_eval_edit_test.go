package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

type harnessEditAgent struct{ dir string }

func (a harnessEditAgent) WorkDir() string                                     { return a.dir }
func (a harnessEditAgent) SessionID() string                                   { return "harness" }
func (a harnessEditAgent) Fire(context.Context, string, any) error             { return nil }
func (a harnessEditAgent) RunSubagent(context.Context, string) (string, error) { return "", nil }

func TestHarnessEditPreservesExecutableMode(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "script.sh")
	if err := os.WriteFile(path, []byte("#!/bin/sh\necho old\n"), 0o751); err != nil {
		t.Fatal(err)
	}
	tool := NewEditTool(harnessEditAgent{dir}, "test")
	args := fmt.Sprintf(`{"filePath":%q,"oldText":"echo old","newText":"echo new"}`, path)
	result, err := tool.Handler.Run(context.Background(), args)
	if err != nil || result.IsError {
		t.Fatalf("edit failed: result=%+v err=%v", result, err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o751 {
		t.Fatalf("mode = %04o, want 0751", got)
	}
	b, _ := os.ReadFile(path)
	if string(b) != "#!/bin/sh\necho new\n" {
		t.Fatalf("content = %q", b)
	}
	matches, _ := filepath.Glob(filepath.Join(dir, ".*script.sh*"))
	if len(matches) != 0 {
		t.Fatalf("temporary files leaked: %v", matches)
	}
}

func TestHarnessEditAtomicallyReplacesInode(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.txt")
	witness := filepath.Join(dir, "old-inode.txt")
	if err := os.WriteFile(path, []byte("value=old\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(path, witness); err != nil {
		t.Fatal(err)
	}
	tool := NewEditTool(harnessEditAgent{dir}, "test")
	args := fmt.Sprintf(`{"filePath":%q,"oldText":"value=old","newText":"value=new"}`, path)
	result, err := tool.Handler.Run(context.Background(), args)
	if err != nil || result.IsError {
		t.Fatalf("edit failed: result=%+v err=%v", result, err)
	}
	old, _ := os.ReadFile(witness)
	if string(old) != "value=old\n" {
		t.Fatalf("old inode was modified in place (%q); replacement was not atomic", old)
	}
	updated, _ := os.ReadFile(path)
	if string(updated) != "value=new\n" {
		t.Fatalf("replacement content = %q", updated)
	}
}
