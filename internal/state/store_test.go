package state

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	s, err := Open(path, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	rec := Record{JobID: 42, RunAttempt: 1, Repo: "acme/widgets", State: StateWaitReady, VMID: "vm-a", QueuedAt: time.Now().UTC()}
	if err := s.Upsert(rec); err != nil {
		t.Fatal(err)
	}

	s2, err := Open(path, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	got, ok := s2.Get(Key{JobID: 42, RunAttempt: 1})
	if !ok || got.VMID != "vm-a" || got.State != StateWaitReady {
		t.Fatalf("reload mismatch: %+v ok=%v", got, ok)
	}
	if !s2.HasVM("vm-a") || s2.HasVM("vm-b") {
		t.Fatal("HasVM wrong")
	}
	if err := s2.Delete(Key{JobID: 42, RunAttempt: 1}); err != nil {
		t.Fatal(err)
	}
	if _, ok := s2.Get(Key{JobID: 42, RunAttempt: 1}); ok {
		t.Fatal("record survived delete")
	}
}

func TestReposDedupes(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "state.json"), slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	for _, rec := range []Record{
		{JobID: 1, RunAttempt: 1, Repo: "acme/widgets", State: StateFailed, QueuedAt: now},
		{JobID: 2, RunAttempt: 1, Repo: "acme/widgets", State: StateQueued, QueuedAt: now},
		{JobID: 3, RunAttempt: 1, Repo: "acme/gadgets", State: StateComplete, QueuedAt: now},
		{JobID: 4, RunAttempt: 1, Repo: "", State: StateFailed, QueuedAt: now},
	} {
		if err := s.Upsert(rec); err != nil {
			t.Fatal(err)
		}
	}
	got := s.Repos()
	if len(got) != 2 || got[0] != "acme/gadgets" || got[1] != "acme/widgets" {
		t.Fatalf("Repos() = %v, want [acme/gadgets acme/widgets]", got)
	}
}

func TestNewIdemKey(t *testing.T) {
	k1 := NewIdemKey(7, 2, 1)
	k2 := NewIdemKey(7, 2, 1)
	if !strings.HasPrefix(k1, "mcra1-j7-a2-r1-") || len(k1) > 128 {
		t.Fatalf("bad key format %q", k1)
	}
	if k1 == k2 {
		t.Fatal("keys for the same attempt must differ (random suffix)")
	}
}

func TestCorruptFileMovedAside(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := Open(path, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	if len(s.List()) != 0 {
		t.Fatal("corrupt store should start empty")
	}
	if _, err := os.Stat(path + ".corrupt"); err != nil {
		t.Fatal("corrupt file not moved aside:", err)
	}
	// Store still works after recovery.
	if err := s.Upsert(Record{JobID: 1, State: StateQueued, QueuedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
}

func TestTerminal(t *testing.T) {
	if StateComplete.Terminal() != true || StateFailed.Terminal() != true || StateJobRunning.Terminal() != false {
		t.Fatal("Terminal() classification wrong")
	}
}

// State written before pools named one image and one region still loads: the
// removed fallback fields are ignored, and the frozen plan simply has neither.
func TestLegacyFallbackPlanLoads(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	legacy := `{"version":2,"records":[{"plan":{"base_images":["preferred","fallback"],"account":"personal",` +
		`"api_base_url":"https://api.github.com","pool":"macos","sku":"m4-titan-v1-special","regions":["east","west"],` +
		`"runner":{},"runner_group_id":1},"region_index":1,"image_index":1,"job_id":7,"run_attempt":1,` +
		`"repo":"acme/widgets","state":"complete"}]}`
	if err := os.WriteFile(path, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := Open(path, slog.Default())
	if err != nil {
		t.Fatalf("legacy state rejected: %v", err)
	}
	records := s.List()
	if len(records) != 1 || records[0].Plan == nil || records[0].Plan.Account != "personal" {
		t.Fatalf("legacy record not loaded: %+v", records)
	}
	if records[0].Plan.BaseImage != "" || records[0].Plan.Region != "" {
		t.Fatalf("legacy fallback lists leaked into the plan: %+v", records[0].Plan)
	}
}
