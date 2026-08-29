package githubapp

import (
	"context"
	"log/slog"
	"strings"
	"time"
)

// JobEvent is a normalized workflow_job signal. The poller emits "queued";
// "in_progress"/"completed" exist for internal signaling and tests.
// ("waiting" — environment protection gating — is dropped at Submit.)
type JobEvent struct {
	Action     string
	Job        JobRef
	Conclusion string
	RunnerName string
}

// Poller periodically lists queued jobs matching our labels and submits
// them as queued events — the sole discovery path (pull-only, no ingress).
type Poller struct {
	Client *Client
	// Repos to watch ("owner/repo"). Always polled, including when
	// DiscoverRepos is set — they are unioned with the recently-pushed set.
	Repos []string
	// DiscoverRepos watches repos the token can access. To stay inside API
	// rate limits with broad tokens, only repos pushed within ActiveWindow
	// are added from discovery (capped at MaxRepos). A rerun does not bump
	// pushed_at, so ExtraRepos (typically repos already in the state file)
	// keeps those quiet repos in the poll set. There is no webhook.
	DiscoverRepos bool
	ActiveWindow  time.Duration
	MaxRepos      int
	OurLabels     []string
	Interval      time.Duration
	Submit        func(JobEvent)
	Log           *slog.Logger
	// ExtraRepos is called each cycle and unioned into the poll set. Nil is
	// fine. Used so a repo we have already served is still polled after it
	// falls out of ActiveWindow.
	ExtraRepos func() []string
}

func (p *Poller) Run(ctx context.Context) {
	t := time.NewTicker(p.Interval)
	defer t.Stop()
	for {
		p.poll(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (p *Poller) poll(ctx context.Context) {
	repos := append([]string(nil), p.Repos...)
	if p.DiscoverRepos {
		infos, err := p.Client.ListAccessibleRepos(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			p.Log.Warn("repo discovery failed", "error", err)
		} else {
			cutoff := time.Now().Add(-p.ActiveWindow)
			n := 0
			for _, ri := range infos { // already sorted most-recently-pushed first
				if ri.PushedAt.Before(cutoff) {
					break
				}
				repos = appendUnique(repos, ri.FullName)
				n++
				if p.MaxRepos > 0 && n >= p.MaxRepos {
					break
				}
			}
		}
	}
	if p.ExtraRepos != nil {
		for _, r := range p.ExtraRepos() {
			repos = appendUnique(repos, r)
		}
	}
	refs, err := p.Client.ListQueuedJobs(ctx, repos, p.OurLabels)
	if err != nil {
		if ctx.Err() == nil {
			p.Log.Warn("github poll failed", "error", err)
		}
		return
	}
	for _, ref := range refs {
		p.Submit(JobEvent{Action: "queued", Job: ref})
	}
}

func appendUnique(dst []string, name string) []string {
	if name == "" {
		return dst
	}
	for _, e := range dst {
		if strings.EqualFold(e, name) {
			return dst
		}
	}
	return append(dst, name)
}
