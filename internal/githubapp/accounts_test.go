package githubapp

import (
	"context"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/Lakr233/mini-control-action/internal/config"
	"github.com/Lakr233/mini-control-action/internal/githubapp/ghfake"
	"github.com/Lakr233/mini-control-action/internal/state"
)

func TestAccountsDiscoverQuietReposAndPersistExplicitOwnership(t *testing.T) {
	srv := ghfake.New()
	defer srv.Close()
	srv.PushedAt = time.Now().Add(-365 * 24 * time.Hour)
	srv.Repos = []string{"acme/widgets", "acme/ignored", "acme/blocked"}
	srv.DeniedRepos = map[string]int{"acme/blocked": 403}
	srv.SetJob(queuedJob())
	cfg := &config.Config{
		GitHub: config.GitHub{APIBaseURL: srv.URL, RunnerGroupID: 1, Poll: config.Poll{ActiveWindow: config.Duration(time.Hour), QuietInterval: config.Duration(time.Minute)}, Accounts: map[string]config.Account{
			"first":  {Token: "first", Exclude: []string{"acme/ignored"}, Defaults: config.Policy{Pool: "default"}},
			"second": {Token: "second", Exclude: []string{"acme/ignored"}, Defaults: config.Policy{Pool: "default"}, Repositories: map[string]config.Policy{"acme/widgets": {}}},
		}},
		Pools:  map[string]config.Pool{"default": {SKU: "m4-big-v1", BaseImage: "example-image", Region: "east", MaxConcurrentVMs: 2}},
		Runner: config.Runner{Labels: labels(), MaxJobDuration: config.Duration(time.Hour)}, Limits: config.Limits{MaxConcurrentVMs: 2},
	}
	storePath := filepath.Join(t.TempDir(), "state.json")
	store, err := state.Open(storePath, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	clients := map[string]*Client{}
	for _, name := range cfg.AccountNames() {
		clients[name], _ = New(cfg.AccountGitHub(name), slog.Default())
	}
	var got []JobEvent
	a := &Accounts{Config: cfg, Clients: clients, Store: store, Log: slog.Default(), Submit: func(ev JobEvent) { got = append(got, ev) }}
	a.Poll(context.Background())
	if len(got) != 1 || got[0].Account != "second" {
		t.Fatalf("quiet repo explicit ownership: %+v", got)
	}
	if len(a.Status()) != 2 {
		t.Fatalf("excluded repo was watched: %+v", a.Status())
	}
	// Removing the override and restarting discovery must not transfer it.
	account := cfg.GitHub.Accounts["second"]
	account.Repositories = nil
	cfg.GitHub.Accounts["second"] = account
	a.Store, err = state.Open(storePath, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	a.lastPoll = nil
	got = nil
	a.Poll(context.Background())
	if len(got) != 1 || got[0].Account != "second" {
		t.Fatalf("ownership changed after restart: %+v", got)
	}
	got = nil
	a.Poll(context.Background())
	if len(got) != 0 {
		t.Fatal("quiet repo polled again before quiet interval")
	}
	for _, status := range a.Status() {
		if status.Repo == "acme/blocked" && status.Status != "repository_access_failed" {
			t.Fatalf("quiet interval erased last error: %+v", status)
		}
	}

}

func TestRepositoryListingOverflowIsError(t *testing.T) {
	srv := ghfake.New()
	defer srv.Close()
	srv.Repos = make([]string, 1000)
	for i := range srv.Repos {
		srv.Repos[i] = "acme/widgets"
	}
	c := testPollerClient(t, srv)
	if _, err := c.ListAccessibleRepos(context.Background()); err == nil {
		t.Fatal("truncated listing accepted")
	}
}
