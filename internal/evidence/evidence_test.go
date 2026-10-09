package evidence

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func jsonUnmarshal(b []byte, v any) error { return json.Unmarshal(b, v) }
func jsonMarshal(v any) ([]byte, error)   { return json.Marshal(v) }

func joinLines(lines []string) string {
	out := ""
	for _, l := range lines {
		out += l + "\n"
	}
	return out
}

func tempTrail(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "trail.jsonl")
}

func TestContentIDIsStable(t *testing.T) {
	a := ContentID(map[string]any{"b": 2, "a": 1})
	b := ContentID(map[string]any{"a": 1, "b": 2})
	if a != b {
		t.Fatalf("content id depends on key order: %s vs %s", a, b)
	}
	if len(a) != 64 {
		t.Fatalf("expected sha256 hex, got %q", a)
	}
}

func TestChainVerifies(t *testing.T) {
	tr, err := Open(tempTrail(t))
	if err != nil {
		t.Fatal(err)
	}
	for i := range 3 {
		if _, err := tr.Record("Agent", "v1", []string{"e1"},
			[]map[string]any{{"id": "r1", "n": i}},
			map[string]any{"verdict": i}, nil); err != nil {
			t.Fatal(err)
		}
	}
	if err := tr.Verify(); err != nil {
		t.Fatalf("verify: %v", err)
	}
	if len(tr.Entries) != 3 {
		t.Fatalf("want 3 entries, got %d", len(tr.Entries))
	}
	if tr.Entries[0].PrevHash != Genesis {
		t.Fatalf("first entry must chain from genesis")
	}
}

func TestTamperingDetected(t *testing.T) {
	path := tempTrail(t)
	tr, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tr.Record("A", "v1", nil, nil, map[string]any{"v": "x"}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := tr.Record("B", "v1", nil, nil, map[string]any{"v": "y"}, nil); err != nil {
		t.Fatal(err)
	}
	// rewrite the first conclusion, keeping the rest of the file intact
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := splitLines(string(raw))
	var first TrailEntry
	if err := jsonUnmarshal([]byte(lines[0]), &first); err != nil {
		t.Fatal(err)
	}
	first.Body["conclusion"] = map[string]any{"v": "tampered"}
	b, err := jsonMarshal(first)
	if err != nil {
		t.Fatal(err)
	}
	lines[0] = string(b)
	if err := os.WriteFile(path, []byte(joinLines(lines)), 0o644); err != nil {
		t.Fatal(err)
	}
	reloaded, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := reloaded.Verify(); err == nil {
		t.Fatal("expected verify to fail after tampering")
	}
}

func TestReloadKeepsChain(t *testing.T) {
	path := tempTrail(t)
	tr, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tr.Record("A", "v1", nil, nil, map[string]any{"v": 1}, nil); err != nil {
		t.Fatal(err)
	}
	again, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(again.Entries) != 1 || again.Head() != tr.Head() {
		t.Fatalf("reload mismatch: %d entries", len(again.Entries))
	}
	if _, err := again.Record("B", "v1", nil, nil, map[string]any{"v": 2}, nil); err != nil {
		t.Fatal(err)
	}
	final, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(final.Entries) != 2 {
		t.Fatalf("want 2 entries after append, got %d", len(final.Entries))
	}
	if err := final.Verify(); err != nil {
		t.Fatalf("verify after reload: %v", err)
	}
}
