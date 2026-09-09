package githubapp

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/Lakr233/mini-control-action/internal/config"
	"github.com/Lakr233/mini-control-action/internal/githubapp/ghfake"
)

func testPollerClient(t *testing.T, srv *ghfake.Server) *Client {
	t.Helper()
	c, err := New(config.GitHub{
		APIBaseURL: srv.URL, Scope: "all", Owner: "acme",
		RunnerGroupID: 1, Auth: config.GitHubAuth{Token: "ghp_test"},
	}, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func labels() []string { return []string{"self-hosted", "macos", "arm64", "mini-control"} }

func queuedJob() *ghfake.Job {
	return &ghfake.Job{
		ID: 10, RunID: 100, RunAttempt: 2, Status: "queued",
		Labels: labels(),
	}
}

func TestPollSkipsQuietRepoOutsideActiveWindow(t *testing.T) {
	srv := ghfake.New()
	t.Cleanup(srv.Close)
	srv.PushedAt = time.Now().UTC().Add(-10 * time.Hour)
	srv.SetJob(queuedJob())

	var got []JobRef
	p := &Poller{
		Client: testPollerClient(t, srv), DiscoverRepos: true,
		ActiveWindow: 2 * time.Hour, MaxRepos: 10, OurLabels: labels(),
		Log:    slog.Default(),
		Submit: func(ev JobEvent) { got = append(got, ev.Job) },
	}
	p.poll(context.Background())
	if len(got) != 0 {
		t.Fatalf("quiet repo outside the push window was polled: %+v", got)
	}
}

func TestPollPicksRerunOnKnownQuietRepo(t *testing.T) {
	srv := ghfake.New()
	t.Cleanup(srv.Close)
	srv.PushedAt = time.Now().UTC().Add(-10 * time.Hour)
	srv.SetJob(queuedJob())

	var got []JobRef
	p := &Poller{
		Client: testPollerClient(t, srv), DiscoverRepos: true,
		ActiveWindow: 2 * time.Hour, MaxRepos: 10, OurLabels: labels(),
		Log:        slog.Default(),
		ExtraRepos: func() []string { return []string{"acme/widgets"} },
		Submit:     func(ev JobEvent) { got = append(got, ev.Job) },
	}
	p.poll(context.Background())
	if len(got) != 1 || got[0].JobID != 10 || got[0].RunAttempt != 2 {
		t.Fatalf("rerun on a known quiet repo was not picked up: %+v", got)
	}
}

func TestPollKeepsConfiguredReposWhenDiscovering(t *testing.T) {
	srv := ghfake.New()
	t.Cleanup(srv.Close)
	srv.PushedAt = time.Now().UTC().Add(-10 * time.Hour)
	srv.SetJob(queuedJob())

	var got []JobRef
	p := &Poller{
		Client: testPollerClient(t, srv), DiscoverRepos: true,
		Repos:        []string{"acme/widgets"},
		ActiveWindow: 2 * time.Hour, MaxRepos: 10, OurLabels: labels(),
		Log:    slog.Default(),
		Submit: func(ev JobEvent) { got = append(got, ev.Job) },
	}
	p.poll(context.Background())
	if len(got) != 1 || got[0].JobID != 10 {
		t.Fatalf("configured repo was dropped during discovery: %+v", got)
	}
}
