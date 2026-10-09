// Package evidence is a Go port of EvidenceTrail's content-addressed evidence
// records and hash-chained reasoning trail (see
// /home/znao/farmo/projects/evidence-trail for the Python original).
//
// Every fact a decision consumes becomes an Evidence record whose ID is the
// SHA-256 of its canonical JSON payload. Decisions are appended to an
// append-only trail chained with SHA-256(prevHash || canonical(body)), so the
// trail is tamper-evident and re-verifiable offline.
package evidence

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Evidence is an immutable, content-addressed observation.
type Evidence struct {
	Kind      string         `json:"kind"`
	Source    string         `json:"source"`
	URL       string         `json:"url"`
	FetchedAt string         `json:"fetched_at"`
	Payload   map[string]any `json:"payload"`
	ID        string         `json:"id"`
}

// Canonical returns the deterministic JSON encoding used for hashing.
func Canonical(v any) string {
	b, err := json.Marshal(v) // encoding/json sorts map keys
	if err != nil {
		panic(fmt.Sprintf("canonical json: %v", err))
	}
	return string(b)
}

// ContentID is the SHA-256 of the canonical payload.
func ContentID(payload map[string]any) string {
	sum := sha256.Sum256([]byte(Canonical(payload)))
	return hex.EncodeToString(sum[:])
}

// Make builds an evidence record and stamps its content ID and fetch time.
func Make(kind, source, url string, payload map[string]any) Evidence {
	return Evidence{
		Kind: kind, Source: source, URL: url,
		FetchedAt: time.Now().UTC().Format(time.RFC3339),
		Payload:   payload,
		ID:        ContentID(payload),
	}
}

// TrailEntry is one reasoning step in the hash chain.
type TrailEntry struct {
	PrevHash string         `json:"prev_hash"`
	Body     map[string]any `json:"body"`
	Hash     string         `json:"hash"`
}

// Trail is the append-only, hash-chained reasoning log.
type Trail struct {
	Path    string
	Entries []TrailEntry
}

const Genesis = "0000000000000000000000000000000000000000000000000000000000000000"

// Open loads an existing trail (or starts a new one) at path.
func Open(path string) (*Trail, error) {
	t := &Trail{Path: path}
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return t, nil
		}
		return nil, err
	}
	for _, line := range splitLines(string(b)) {
		var e TrailEntry
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			return nil, fmt.Errorf("corrupt trail line: %w", err)
		}
		t.Entries = append(t.Entries, e)
	}
	return t, nil
}

func splitLines(s string) []string {
	var out []string
	start := 0
	for i := range s {
		if s[i] == '\n' {
			if i > start {
				out = append(out, s[start:i])
			}
			start = i + 1
		}
	}
	if start < len(s) {
		out = append(out, s[start:])
	}
	return out
}

// Head returns the hash of the newest entry (or the genesis hash).
func (t *Trail) Head() string {
	if len(t.Entries) == 0 {
		return Genesis
	}
	return t.Entries[len(t.Entries)-1].Hash
}

func hashEntry(prev string, body map[string]any) string {
	sum := sha256.Sum256([]byte(prev + Canonical(body)))
	return hex.EncodeToString(sum[:])
}

// Record appends a reasoning step and persists it.
// agent/ruleset/evidenceIDs/rules/conclusion mirror the Python trail schema.
func (t *Trail) Record(agent, ruleset string, evidenceIDs []string, rules []map[string]any,
	conclusion map[string]any, inputs map[string]any) (TrailEntry, error) {
	body := map[string]any{
		"seq":          len(t.Entries) + 1,
		"ts":           time.Now().UTC().Format(time.RFC3339),
		"agent":        agent,
		"ruleset":      ruleset,
		"evidence_ids": evidenceIDs,
		"rules":        rules,
		"inputs":       inputs,
		"conclusion":   conclusion,
	}
	// freeze: callers must not be able to mutate the recorded body afterwards
	var frozen map[string]any
	if err := json.Unmarshal([]byte(Canonical(body)), &frozen); err != nil {
		return TrailEntry{}, err
	}
	e := TrailEntry{PrevHash: t.Head(), Body: frozen}
	e.Hash = hashEntry(e.PrevHash, frozen)
	t.Entries = append(t.Entries, e)
	if err := t.appendFile(e); err != nil {
		return TrailEntry{}, err
	}
	return e, nil
}

func (t *Trail) appendFile(e TrailEntry) error {
	if err := os.MkdirAll(filepath.Dir(t.Path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(t.Path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	_, err = f.Write(append(b, '\n'))
	return err
}

// Verify recomputes the whole chain.
func (t *Trail) Verify() error {
	prev := Genesis
	for i, e := range t.Entries {
		n := i + 1
		if e.PrevHash != prev {
			return fmt.Errorf("entry %d: prev_hash mismatch", n)
		}
		if seq, _ := e.Body["seq"].(float64); int(seq) != n {
			return fmt.Errorf("entry %d: sequence mismatch", n)
		}
		if hashEntry(prev, e.Body) != e.Hash {
			return fmt.Errorf("entry %d: hash mismatch (trail was modified)", n)
		}
		prev = e.Hash
	}
	return nil
}

// HeadShort returns a 16-char preview of the head hash for logging.
func (t *Trail) HeadShort() string {
	h := t.Head()
	if len(h) > 16 {
		return h[:16]
	}
	return h
}
