package scaler

import (
	"fmt"
	"strings"

	"github.com/Lakr233/mini-control-action/internal/githubapp"
	"github.com/Lakr233/mini-control-action/internal/state"
)

// ConfigureAccounts runs before Resume or reconciliation. Missing credentials
// or incompatible persisted plans must stop startup rather than orphan VMs.
func (s *Scaler) ConfigureAccounts(clients map[string]*githubapp.Client) error {
	for _, rec := range s.store.List() {
		if rec.Plan == nil {
			if !rec.State.Terminal() {
				return fmt.Errorf("legacy jobs are still active; drain the old deployment before enabling accounts")
			}
			continue
		}
		if rec.State.Terminal() {
			continue
		}
		plan := rec.Plan
		if clients[plan.Account] == nil {
			return fmt.Errorf("active job requires missing account %q", plan.Account)
		}
		current := s.cfg.AccountGitHub(plan.Account)
		if !strings.EqualFold(strings.TrimRight(current.APIBaseURL, "/"), strings.TrimRight(plan.APIBaseURL, "/")) || current.RunnerGroupID != plan.RunnerGroupID {
			return fmt.Errorf("active job requires original GitHub host and runner group for account %q", plan.Account)
		}
		if _, ok := s.cfg.Pools[plan.Pool]; !ok {
			return fmt.Errorf("active job requires missing pool %q", plan.Pool)
		}
		if len(plan.Regions) == 0 || rec.RegionIndex < 0 || rec.RegionIndex >= len(plan.Regions) {
			return fmt.Errorf("invalid persisted region for job %d", rec.JobID)
		}
	}
	s.clients = clients
	return nil
}

func (s *Scaler) AdmissionStatus() map[string]string { return s.admission.status() }

// GitHubForRecord keeps cleanup tied to the credential that registered the runner.
func (s *Scaler) GitHubForRecord(rec state.Record) *githubapp.Client {
	if rec.Plan != nil {
		return s.clients[rec.Plan.Account]
	}
	return s.gh
}

// ReleaseReservation is called only after cleanup has confirmed VM absence.
func (s *Scaler) ReleaseReservation(key state.Key) { s.admission.release(key) }
