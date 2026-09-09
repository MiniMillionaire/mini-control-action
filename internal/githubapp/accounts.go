package githubapp

import (
	"context"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Lakr233/mini-control-action/internal/config"
	"github.com/Lakr233/mini-control-action/internal/state"
)

func RepositoryNamespace(base, repo string) string {
	return strings.ToLower(strings.TrimRight(base, "/") + "/" + repo)
}

type RepositoryStatus struct {
	Account string `json:"account"`
	Repo    string `json:"repo"`
	Pool    string `json:"pool,omitempty"`
	Status  string `json:"status"`
}

// Accounts discovers every candidate before deciding ownership, so network
// response order cannot select credentials. Persistent bindings never fail
// over silently when a token loses permissions or discovery fails.
type Accounts struct {
	Config   *config.Config
	Clients  map[string]*Client
	Store    *state.Store
	Submit   func(JobEvent)
	Log      *slog.Logger
	mu       sync.Mutex
	statuses map[string]RepositoryStatus
	lastPoll map[string]time.Time
}

func (a *Accounts) Status() []RepositoryStatus {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]RepositoryStatus, 0, len(a.statuses))
	for _, status := range a.statuses {
		out = append(out, status)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Account != out[j].Account {
			return out[i].Account < out[j].Account
		}
		return out[i].Repo < out[j].Repo
	})
	return out
}

func (a *Accounts) Run(ctx context.Context) {
	ticker := time.NewTicker(a.Config.GitHub.Poll.Interval.D())
	defer ticker.Stop()
	for {
		a.Poll(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (a *Accounts) Poll(ctx context.Context) {
	if a.lastPoll == nil {
		a.lastPoll = map[string]time.Time{}
	}
	a.mu.Lock()
	previous := a.statuses
	a.mu.Unlock()
	names := a.Config.AccountNames()
	statuses := map[string]RepositoryStatus{}
	candidates := map[string][]string{}
	repos := map[string]string{}
	active := map[string]bool{}
	explicit := map[string]string{}
	discoveryFailed := map[string]bool{}
	add := func(name, repo string, frequent bool) {
		account := a.Config.GitHub.Accounts[name]
		if !account.Includes(repo) {
			return
		}
		ns := RepositoryNamespace(a.Config.AccountGitHub(name).APIBaseURL, repo)
		candidates[ns] = appendUnique(candidates[ns], name)
		repos[ns] = strings.ToLower(repo)
		active[ns] = active[ns] || frequent
	}
	for _, name := range names {
		account := a.Config.GitHub.Accounts[name]
		for repo := range account.Repositories {
			ns := RepositoryNamespace(a.Config.AccountGitHub(name).APIBaseURL, repo)
			explicit[ns] = name // exclusion still wins in add/Resolve
			add(name, repo, true)
		}
		if account.Discover != "none" {
			infos, err := a.Clients[name].ListAccessibleRepos(ctx)
			if err != nil {
				discoveryFailed[name] = true
				a.Log.Warn("account discovery failed", "account", name, "error", err)
				statuses["account:"+name] = RepositoryStatus{Account: name, Status: "discovery_failed"}
			} else {
				for _, info := range infos {
					add(name, info.FullName, time.Since(info.PushedAt) <= a.Config.GitHub.Poll.ActiveWindow.D())
				}
			}
		}
	}
	for _, rec := range a.Store.List() {
		if rec.Plan != nil {
			if _, ok := a.Config.GitHub.Accounts[rec.Plan.Account]; ok {
				add(rec.Plan.Account, rec.Repo, !rec.State.Terminal())
			}
		}
	}
	byAccount := map[string][]string{}
	for ns, choices := range candidates {
		owner := a.Store.RepositoryOwner(ns)
		if owner == "" {
			owner = explicit[ns]
		}
		if owner == "" {
			for _, name := range names {
				for _, candidate := range choices {
					if candidate == name {
						owner = name
						break
					}
				}
				if owner != "" {
					break
				}
			}
		}
		plan, _, paused, err := a.Config.Resolve(owner, repos[ns])
		pending := false
		if a.Store.RepositoryOwner(ns) == "" && explicit[ns] == "" {
			for _, name := range names {
				if name == owner {
					break
				}
				candidate := a.Config.GitHub.Accounts[name]
				if discoveryFailed[name] && candidate.Includes(repos[ns]) && strings.EqualFold(a.Config.AccountGitHub(name).APIBaseURL, plan.APIBaseURL) {
					pending = true
				}
			}
		}
		status := RepositoryStatus{Account: owner, Repo: repos[ns], Pool: plan.Pool, Status: "watching"}
		switch {
		case pending:
			status.Status = "discovery_pending"
		case err != nil:
			status.Status = "configuration_missing"
		case explicit[ns] != "" && explicit[ns] != owner:
			status.Status = "ownership_conflict"
		case paused:
			status.Status = "paused"
		}
		statuses[ns] = status
		if status.Status != "watching" {
			continue
		}
		if !active[ns] && time.Since(a.lastPoll[ns]) < a.Config.GitHub.Poll.QuietInterval.D() {
			if last, ok := previous[ns]; ok {
				statuses[ns] = last
			}
			continue
		}
		if err := a.Store.BindRepository(ns, owner); err != nil {
			a.Log.Error("persist repository ownership", "repo", repos[ns], "error", err)
			status.Status = "state_write_failed"
			statuses[ns] = status
			continue
		}
		byAccount[owner] = append(byAccount[owner], repos[ns])
		a.lastPoll[ns] = time.Now()
	}
	for _, name := range names {
		watched := byAccount[name]
		sort.Strings(watched)
		refs, failures, err := a.Clients[name].listQueuedJobsWithErrors(ctx, watched, a.Config.Runner.Labels)
		for repo := range failures {
			ns := RepositoryNamespace(a.Config.AccountGitHub(name).APIBaseURL, repo)
			status := statuses[ns]
			status.Status = "repository_access_failed"
			statuses[ns] = status
		}
		if err != nil {
			continue
		}
		checked := map[string]bool{}
		for _, ref := range refs {
			ns := RepositoryNamespace(a.Config.AccountGitHub(name).APIBaseURL, ref.Repo)
			if !checked[ref.Repo] {
				// Listing runner registrations checks access without creating a
				// billed VM. GitHub does not expose a complete PAT write-scope probe.
				if _, err := a.Clients[name].ListRunners(ctx, ref.Repo); err != nil {
					status := statuses[ns]
					status.Status = "runner_access_failed"
					statuses[ns] = status
					a.Log.Warn("runner access check failed", "account", name, "repo", ref.Repo, "error", err)
					continue
				}
				checked[ref.Repo] = true
			}
			a.Submit(JobEvent{Account: name, Action: "queued", Job: ref})
		}
	}
	a.mu.Lock()
	a.statuses = statuses
	a.mu.Unlock()
}
