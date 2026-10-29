// Package state persists per-job runner records to a single JSON file with
// atomic writes, so the service can resume VM lifecycles after a restart and
// the reconciler can tell owned VMs from orphans.
package state

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/Lakr233/mini-control-action/internal/config"
)

// JobState is the FSM state of one workflow job's runner VM.
type JobState string

const (
	StateQueued        JobState = "queued"
	StateCreating      JobState = "creating"
	StateWaitReady     JobState = "wait_ready"
	StateBootstrapping JobState = "bootstrapping"
	StateRunnerUp      JobState = "runner_up"
	StateJobRunning    JobState = "job_running"
	StateJobDone       JobState = "job_done"
	StateDeleting      JobState = "deleting"
	StateComplete      JobState = "complete"
	StateFailed        JobState = "failed" // terminal failure; VM already deleted
)

// Terminal reports whether s needs no further work.
func (s JobState) Terminal() bool { return s == StateComplete || s == StateFailed }

type Record struct {
	Namespace      string       `json:"namespace,omitempty"`
	Plan           *config.Plan `json:"plan,omitempty"`
	RegionIndex    int          `json:"region_index,omitempty"`
	ImageIndex     int          `json:"image_index,omitempty"`
	JobID          int64        `json:"job_id"`
	RunAttempt     int          `json:"run_attempt"`
	Retry          int          `json:"retry"`
	Repo           string       `json:"repo"` // "owner/repo"
	Labels         []string     `json:"labels"`
	WorkflowJobURL string       `json:"workflow_job_url,omitempty"`

	State          JobState  `json:"state"`
	VMID           string    `json:"vm_id,omitempty"`
	IdempotencyKey string    `json:"idempotency_key,omitempty"`
	RunnerName     string    `json:"runner_name,omitempty"`
	RunnerID       int64     `json:"runner_id,omitempty"`
	LastError      string    `json:"last_error,omitempty"`
	QueuedAt       time.Time `json:"queued_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}

// Key identifies a job attempt uniquely.
type Key struct {
	JobID      int64
	RunAttempt int
	Namespace  string
}

func (r *Record) Key() Key {
	return Key{JobID: r.JobID, RunAttempt: r.RunAttempt, Namespace: r.Namespace}
}

// NewIdemKey builds the Idempotency-Key for one create attempt. The random
// suffix guarantees a fresh key even if the state file was lost and counters
// reset (a purely deterministic key would replay a stale create response
// pointing at a long-deleted VM). Crash-replay safety comes from persisting
// the key in the record BEFORE the create call, not from determinism.
func NewIdemKey(jobID int64, attempt, retry int) string {
	return fmt.Sprintf("mcra1-j%d-a%d-r%d-%s", jobID, attempt, retry, ShortNonce())
}

// ShortNonce returns 8 hex chars of randomness for names/keys that must not
// collide with earlier incarnations of themselves.
func ShortNonce() string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

type Store struct {
	mu     sync.Mutex
	path   string
	recs   map[Key]Record
	owners map[string]string
	log    *slog.Logger
}

type fileFormat struct {
	Version int               `json:"version"`
	Records []Record          `json:"records"`
	Owners  map[string]string `json:"owners,omitempty"`
}

// Open loads (or initializes) the store at path. A corrupt file is moved
// aside to <path>.corrupt and the store starts fresh with a loud error log —
// the reconciler will re-adopt or delete any VMs the lost records tracked.
func Open(path string, log *slog.Logger) (*Store, error) {
	s := &Store{path: path, recs: map[Key]Record{}, owners: map[string]string{}, log: log}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	var f fileFormat
	if err := json.Unmarshal(raw, &f); err != nil {
		corrupt := path + ".corrupt"
		log.Error("state file is corrupt; moving aside and starting fresh — "+
			"the reconciler will clean up any VMs it tracked", "path", path, "moved_to", corrupt, "error", err)
		if mvErr := os.Rename(path, corrupt); mvErr != nil {
			return nil, fmt.Errorf("state file corrupt (%v) and could not move aside: %w", err, mvErr)
		}
		return s, nil
	}
	if f.Version > 2 {
		return nil, fmt.Errorf("unsupported state version %d", f.Version)
	}
	if f.Owners != nil {
		s.owners = f.Owners
	}
	for _, r := range f.Records {
		s.recs[r.Key()] = r
	}
	return s, nil
}

// persist writes atomically: temp file in the same directory, then rename.
// Caller must hold mu.
func (s *Store) persist() error {
	f := fileFormat{Version: 2, Owners: s.owners, Records: make([]Record, 0, len(s.recs))}
	for _, r := range s.recs {
		f.Records = append(f.Records, r)
	}
	sort.Slice(f.Records, func(i, j int) bool {
		if f.Records[i].Namespace != f.Records[j].Namespace {
			return f.Records[i].Namespace < f.Records[j].Namespace
		}
		if f.Records[i].JobID != f.Records[j].JobID {
			return f.Records[i].JobID < f.Records[j].JobID
		}
		return f.Records[i].RunAttempt < f.Records[j].RunAttempt
	})
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.path), ".state-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	return os.Rename(tmpName, s.path)
}

// Upsert stores rec (stamping UpdatedAt) and persists.
func (s *Store) Upsert(rec Record) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec.UpdatedAt = time.Now().UTC()
	previous, existed := s.recs[rec.Key()]
	s.recs[rec.Key()] = cloneRecord(rec)
	if err := s.persist(); err != nil {
		if existed {
			s.recs[rec.Key()] = previous
		} else {
			delete(s.recs, rec.Key())
		}
		return err
	}
	return nil
}

func (s *Store) Get(k Key) (Record, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.recs[k]
	return cloneRecord(r), ok
}

// Repos returns unique owner/repo names from stored records, sorted.
func (s *Store) Repos() []string {
	seen := map[string]struct{}{}
	var out []string
	for _, r := range s.List() {
		if r.Repo == "" {
			continue
		}
		if _, ok := seen[r.Repo]; ok {
			continue
		}
		seen[r.Repo] = struct{}{}
		out = append(out, r.Repo)
	}
	sort.Strings(out)
	return out
}

// List returns all records, newest-queued first.
func (s *Store) List() []Record {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Record, 0, len(s.recs))
	for _, r := range s.recs {
		out = append(out, cloneRecord(r))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].QueuedAt.After(out[j].QueuedAt) })
	return out
}

// Delete removes a record and persists.
func (s *Store) Delete(k Key) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.recs, k)
	return s.persist()
}

// SetUpdatedAtForTest backdates a record's UpdatedAt (test-only hook for
// retention pruning).
func (s *Store) SetUpdatedAtForTest(k Key, at time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r, ok := s.recs[k]; ok {
		r.UpdatedAt = at
		s.recs[k] = r
	}
}

// HasVM reports whether any record references vmID.
func (s *Store) HasVM(vmID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range s.recs {
		if r.VMID == vmID && !r.State.Terminal() {
			return true
		}
	}
	return false
}

// RepositoryOwner is durable across job retention and restarts.
func (s *Store) RepositoryOwner(namespace string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.owners[namespace]
}

func (s *Store) BindRepository(namespace, account string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.owners[namespace] == account {
		return nil
	}
	if old := s.owners[namespace]; old != "" && old != account {
		return fmt.Errorf("repository already belongs to account %q", old)
	}
	s.owners[namespace] = account
	if err := s.persist(); err != nil {
		delete(s.owners, namespace)
		return err
	}
	return nil
}

func cloneRecord(r Record) Record {
	r.Labels = append([]string(nil), r.Labels...)
	if r.Plan != nil {
		p := *r.Plan
		p.Regions = append([]string(nil), p.Regions...)
		p.BaseImages = append([]string(nil), p.BaseImages...)
		p.Runner.Labels = append([]string(nil), p.Runner.Labels...)
		r.Plan = &p
	}
	return r
}
