package config

import (
	"fmt"
	"net/url"
	"path"
	"regexp"
	"sort"
	"strings"
)

// Account names are stable credential references; secrets never enter state.
// Priority, then name, determines ownership of overlapping discoveries.
type Account struct {
	Token        string            `yaml:"token"`
	APIBaseURL   string            `yaml:"api_base_url"`
	Discover     string            `yaml:"discover"` // all (default) or none
	Priority     int               `yaml:"priority"`
	Owners       []string          `yaml:"owners"`  // optional owner/org allowlist
	Exclude      []string          `yaml:"exclude"` // owner/repo globs
	Paused       bool              `yaml:"paused"`
	Defaults     Policy            `yaml:"defaults"`
	Repositories map[string]Policy `yaml:"repositories"`
}

type Pool struct {
	BaseImage        string   `yaml:"base_image"`
	SKU              string   `yaml:"sku"`
	Regions          []string `yaml:"regions"`
	AnyRegion        bool     `yaml:"any_region"`
	MaxConcurrentVMs int      `yaml:"max_concurrent_vms"`
	Paused           bool     `yaml:"paused"`
}

// Pointers distinguish inherited settings from explicit zero/false values.
type Policy struct {
	Pool             string   `yaml:"pool"`
	Regions          []string `yaml:"regions"`
	MaxConcurrentVMs *int     `yaml:"max_concurrent_vms"`
	MaxJobDuration   Duration `yaml:"max_job_duration"`
	Paused           *bool    `yaml:"paused"`
}

// Plan freezes non-secret execution settings for recovery. Current limits and
// pause flags are admission policy, so lowering them also affects queued work.
type Plan struct {
	BaseImage     string   `json:"base_image,omitempty"`
	Account       string   `json:"account"`
	APIBaseURL    string   `json:"api_base_url"`
	Pool          string   `json:"pool"`
	SKU           string   `json:"sku"`
	Regions       []string `json:"regions"`
	Runner        Runner   `json:"runner"`
	RunnerGroupID int64    `json:"runner_group_id"`
}

func (c *Config) AccountNames() []string {
	names := make([]string, 0, len(c.GitHub.Accounts))
	for name := range c.GitHub.Accounts {
		names = append(names, name)
	}
	sort.Slice(names, func(i, j int) bool {
		a, b := c.GitHub.Accounts[names[i]], c.GitHub.Accounts[names[j]]
		if a.Priority != b.Priority {
			return a.Priority < b.Priority
		}
		return names[i] < names[j]
	})
	return names
}

func (c *Config) AccountGitHub(name string) GitHub {
	a := c.GitHub.Accounts[name]
	g := c.GitHub
	g.Accounts = nil
	g.Scope, g.Owner, g.Repo = "all", "", ""
	g.Auth.Token = a.Token
	if a.APIBaseURL != "" {
		g.APIBaseURL = a.APIBaseURL
	}
	return g
}

func (a Account) Includes(repo string) bool {
	repo = strings.ToLower(repo)
	if len(a.Owners) > 0 {
		owner, _, _ := strings.Cut(repo, "/")
		found := false
		for _, candidate := range a.Owners {
			found = found || strings.EqualFold(owner, candidate)
		}
		if !found {
			return false
		}
	}
	for _, pattern := range a.Exclude {
		if match, _ := path.Match(strings.ToLower(pattern), repo); match {
			return false
		}
	}
	return true
}

func mergePolicy(base, override Policy) Policy {
	if override.Pool != "" {
		base.Pool = override.Pool
	}
	if override.Regions != nil {
		base.Regions = override.Regions
	}
	if override.MaxConcurrentVMs != nil {
		base.MaxConcurrentVMs = override.MaxConcurrentVMs
	}
	if override.MaxJobDuration != 0 {
		base.MaxJobDuration = override.MaxJobDuration
	}
	if override.Paused != nil {
		base.Paused = override.Paused
	}
	return base
}

func (c *Config) Resolve(account, repo string) (Plan, int, bool, error) {
	a, ok := c.GitHub.Accounts[account]
	if !ok {
		return Plan{}, 0, false, fmt.Errorf("unknown account %q", account)
	}
	p := mergePolicy(c.Defaults, a.Defaults)
	for name, override := range a.Repositories {
		if strings.EqualFold(name, repo) {
			p = mergePolicy(p, override)
		}
	}
	pool, ok := c.Pools[p.Pool]
	if !ok {
		return Plan{}, 0, false, fmt.Errorf("account %q repo %q references unknown pool %q", account, repo, p.Pool)
	}
	regions := pool.Regions
	if pool.AnyRegion {
		regions = []string{""}
	}
	if p.Regions != nil {
		if len(p.Regions) == 0 {
			return Plan{}, 0, false, fmt.Errorf("regions must not be empty")
		}
		for _, region := range p.Regions {
			found := pool.AnyRegion
			for _, allowed := range pool.Regions {
				found = found || strings.EqualFold(region, allowed)
			}
			if strings.TrimSpace(region) == "" || !found {
				return Plan{}, 0, false, fmt.Errorf("region %q is outside pool %q", region, p.Pool)
			}
		}
		regions = p.Regions
	}
	limit := c.Limits.MaxConcurrentVMs
	if p.MaxConcurrentVMs != nil {
		limit = *p.MaxConcurrentVMs
	}
	if limit < 1 {
		return Plan{}, 0, false, fmt.Errorf("max_concurrent_vms must be >= 1; use paused to stop admission")
	}
	runner := c.Runner
	if p.MaxJobDuration != 0 {
		runner.MaxJobDuration = p.MaxJobDuration
	}
	if runner.MaxJobDuration <= 0 {
		return Plan{}, 0, false, fmt.Errorf("max_job_duration must be positive")
	}
	paused := a.Paused || pool.Paused || !a.Includes(repo) || (p.Paused != nil && *p.Paused)
	return Plan{BaseImage: pool.BaseImage, Account: account, APIBaseURL: c.AccountGitHub(account).APIBaseURL, Pool: p.Pool, SKU: pool.SKU, Regions: append([]string(nil), regions...), Runner: runner, RunnerGroupID: c.GitHub.RunnerGroupID}, limit, paused, nil
}

func (c *Config) validateAccounts() error {
	if len(c.GitHub.Accounts) == 0 {
		return nil
	}
	if c.GitHub.Auth.Token != "" || c.GitHub.Scope != "all" || len(c.GitHub.Poll.Repos) != 0 {
		return fmt.Errorf("github.accounts cannot be combined with legacy auth, scope repo/org, or poll.repos")
	}
	if c.GitHub.Poll.Interval <= 0 || c.GitHub.Poll.QuietInterval <= 0 {
		return fmt.Errorf("github.poll.interval must be positive")
	}
	for name, p := range c.Pools {
		if p.BaseImage != "" && (len(p.BaseImage) > 128 || !regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`).MatchString(p.BaseImage)) {
			return fmt.Errorf("pool %q has invalid base_image", name)
		}
		if name == "" || p.SKU == "" || p.MaxConcurrentVMs < 1 {
			return fmt.Errorf("pool %q requires sku and max_concurrent_vms >= 1", name)
		}
		if p.AnyRegion == (len(p.Regions) > 0) {
			return fmt.Errorf("pool %q requires either nonempty regions or any_region: true", name)
		}
		seen := map[string]bool{}
		for _, region := range p.Regions {
			key := strings.ToLower(region)
			if strings.TrimSpace(region) != region || region == "" || len(region) > 32 || strings.Contains(region, ",") || seen[key] {
				return fmt.Errorf("pool %q has empty, padded, or duplicate region", name)
			}
			seen[key] = true
		}
	}
	explicit := map[string]string{}
	for name, a := range c.GitHub.Accounts {
		if name == "" || a.Token == "" {
			return fmt.Errorf("account %q token is required", name)
		}
		if a.Discover != "" && a.Discover != "all" && a.Discover != "none" {
			return fmt.Errorf("account %q discover must be all or none", name)
		}
		u, err := url.Parse(c.AccountGitHub(name).APIBaseURL)
		if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil {
			return fmt.Errorf("account %q has invalid api_base_url", name)
		}
		for _, pattern := range a.Exclude {
			if _, err := path.Match(pattern, "owner/repo"); err != nil {
				return fmt.Errorf("account %q invalid exclude pattern", name)
			}
		}
		if _, _, _, err := c.Resolve(name, ""); err != nil {
			return err
		}
		for repo := range a.Repositories {
			parts := strings.Split(repo, "/")
			if len(parts) != 2 || parts[0] == "" || parts[1] == "" || strings.ContainsAny(repo, " *?[]\\\t\n") {
				return fmt.Errorf("invalid repository %q", repo)
			}
			key := strings.ToLower(strings.TrimRight(u.String(), "/") + "/" + repo)
			if previous := explicit[key]; previous != "" {
				return fmt.Errorf("repository %q configured more than once (%s, %s)", repo, previous, name)
			}
			explicit[key] = name
			if _, _, _, err := c.Resolve(name, repo); err != nil {
				return err
			}
		}
	}
	return nil
}
