package tools

import (
	"context"
	"fmt"
	"sync"
	"testing"
)

func TestHarnessRegistrySnapshotIsolation(t *testing.T) {
	r := NewRegistry()
	r.Register(&ToolDef{Name: "one"})
	snapshot := r.Tools()
	delete(snapshot, "one")
	snapshot["fake"] = &ToolDef{Name: "fake"}
	if _, ok := r.Lookup("one"); !ok {
		t.Fatal("mutating Tools snapshot removed registry entry")
	}
	if _, ok := r.Lookup("fake"); ok {
		t.Fatal("mutating Tools snapshot added registry entry")
	}
}

func TestHarnessRegistryConcurrentAccess(t *testing.T) {
	r := NewRegistry()
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 250; i++ {
				name := fmt.Sprintf("tool-%d-%d", g, i)
				r.Register(&ToolDef{Name: name})
				r.Lookup(name)
				_ = r.Tools()
				_, _ = r.Execute(context.Background(), "missing", "{}")
			}
		}(g)
	}
	wg.Wait()
}
