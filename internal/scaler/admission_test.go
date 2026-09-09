package scaler

import (
	"context"
	"testing"
	"time"

	"github.com/Lakr233/mini-control-action/internal/config"
	"github.com/Lakr233/mini-control-action/internal/state"
)

func TestRecoveryCountsExistingVMsBeforeAdmittingNewWork(t *testing.T) {
	cfg := &config.Config{Limits: config.Limits{MaxConcurrentVMs: 1}}
	a := newAdmission(cfg)
	old := state.Record{JobID: 1, RunAttempt: 1, VMID: "existing", State: state.StateRunnerUp}
	a.seed([]state.Record{old})
	// Recovery must bypass a lowered limit so existing VMs remain monitored.
	if err := a.acquire(context.Background(), old); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := a.acquire(ctx, state.Record{JobID: 2, RunAttempt: 1}); err == nil {
		t.Fatal("new VM bypassed recovered reservation")
	}
	a.release(old.Key())
	ctx2, cancel2 := context.WithTimeout(context.Background(), time.Second)
	defer cancel2()
	if err := a.acquire(ctx2, state.Record{JobID: 2, RunAttempt: 1}); err != nil {
		t.Fatal(err)
	}
}
