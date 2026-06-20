package main

import (
	"io"
	"log/slog"
	"path/filepath"
	"testing"

	"github.com/trick77/relumetv/internal/config"
)

// newTestSetup builds a setupStatus over a fresh (uncommitted) temp config, with a
// controllable active() signal and a commit counter.
func newTestSetup(t *testing.T) (*setupStatus, *config.Config, *bool, *int) {
	t.Helper()
	cfg, err := config.Load(filepath.Join(t.TempDir(), "relumetv.json"))
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	active := false
	commits := 0
	st := newSetupStatus(cfg, func() bool { return active },
		func() error { commits++; return cfg.Commit() },
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	return st, cfg, &active, &commits
}

func pairPro(t *testing.T, cfg *config.Config) {
	t.Helper()
	if err := cfg.SetPro(&config.BridgePro{Host: "10.0.0.5", AppKey: "k"}); err != nil {
		t.Fatalf("SetPro: %v", err)
	}
}

func pairTV(t *testing.T, cfg *config.Config) {
	t.Helper()
	if err := cfg.AddApiUser(&config.ApiUser{Username: "u1", DeviceType: "TV#x"}); err != nil {
		t.Fatalf("AddApiUser: %v", err)
	}
}

func TestSetup_StartsAtStepOne(t *testing.T) {
	st, _, _, _ := newTestSetup(t)
	if got := st.CurrentStep(); got != stepPairPro {
		t.Fatalf("initial step = %d, want %d", got, stepPairPro)
	}
}

func TestSetup_CommittedConfigStartsDone(t *testing.T) {
	cfg, _ := config.Load(filepath.Join(t.TempDir(), "relumetv.json"))
	if err := cfg.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	st := newSetupStatus(cfg, func() bool { return false }, func() error { return nil },
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	if got := st.CurrentStep(); got != stepDone {
		t.Fatalf("step for a committed config = %d, want stepDone", got)
	}
}

// TestSetup_Step3NoFlickerWhenProUpAtStart is advisor blocking #1: at step 3 with the
// Pro reachable (the normal state, since step 1 paired it), the machine must NOT advance
// — it waits for the reachable->unreachable transition.
func TestSetup_Step3NoFlickerWhenProUpAtStart(t *testing.T) {
	st, cfg, _, _ := newTestSetup(t)
	pairPro(t, cfg)
	st.recomputeNow()         // step 1 -> 2
	st.markTVDescriptorSeen() // step 2 -> 3
	if got := st.CurrentStep(); got != stepProPowerOff {
		t.Fatalf("step = %d, want %d (at power-off step)", got, stepProPowerOff)
	}
	// The poller reports the Pro is up (t=0): must stay on step 3, no flicker forward.
	st.setReachable(true)
	if got := st.CurrentStep(); got != stepProPowerOff {
		t.Fatalf("step flickered to %d on a reachable Pro; must stay at step 3", got)
	}
	st.setReachable(true) // still up
	if got := st.CurrentStep(); got != stepProPowerOff {
		t.Fatalf("step advanced to %d on a still-reachable Pro", got)
	}
}

// TestSetup_FullHappyPath walks all six steps in order and asserts the deferred commit
// fires exactly once at the end.
func TestSetup_FullHappyPath(t *testing.T) {
	st, cfg, active, commits := newTestSetup(t)

	// Step 1 -> 2: Pro paired.
	pairPro(t, cfg)
	st.recomputeNow()
	if got := st.CurrentStep(); got != stepRebootTV {
		t.Fatalf("after pairing step = %d, want %d", got, stepRebootTV)
	}

	// Step 2 -> 3: TV fetched the descriptor.
	st.markTVDescriptorSeen()
	if got := st.CurrentStep(); got != stepProPowerOff {
		t.Fatalf("after descriptor step = %d, want %d", got, stepProPowerOff)
	}

	// Poller confirms the Pro is up, then it goes down -> step 3 -> 4.
	st.setReachable(true)
	st.setReachable(false)
	if got := st.CurrentStep(); got != stepTVScan {
		t.Fatalf("after power-off step = %d, want %d", got, stepTVScan)
	}

	// Step 4 -> 5: the TV pairs while the Pro is off.
	pairTV(t, cfg)
	st.recomputeNow()
	if got := st.CurrentStep(); got != stepProPowerOn {
		t.Fatalf("after TV pairing step = %d, want %d", got, stepProPowerOn)
	}

	// Step 5 -> 6: the Pro comes back.
	st.setReachable(true)
	if got := st.CurrentStep(); got != stepAssignBulbs {
		t.Fatalf("after power-on step = %d, want %d", got, stepAssignBulbs)
	}
	if *commits != 0 {
		t.Fatalf("committed before the final step (%d commits)", *commits)
	}

	// Step 6 -> done: first TV activity drives the lights.
	*active = true
	st.recomputeNow()
	if got := st.CurrentStep(); got != stepDone {
		t.Fatalf("after first activity step = %d, want stepDone", got)
	}
	if *commits != 1 {
		t.Fatalf("commit count = %d, want exactly 1", *commits)
	}
	if !cfg.Committed() {
		t.Fatal("config not committed at setup completion")
	}

	// Idempotent: further signals must not re-commit.
	st.recomputeNow()
	st.setReachable(false)
	if *commits != 1 {
		t.Fatalf("commit fired again: count = %d", *commits)
	}
}

// TestSetup_Step5OnlyAfterStep3 ensures step 5's reachable check is gated behind step 3:
// a reachable Pro during steps before 5 must not be mistaken for "Pro back on".
func TestSetup_Step5OnlyAfterStep3(t *testing.T) {
	st, cfg, _, _ := newTestSetup(t)
	pairPro(t, cfg)
	st.recomputeNow()
	st.markTVDescriptorSeen() // now at step 3
	// Many reachable reports while at step 3 must never jump to step 5/6.
	for i := 0; i < 5; i++ {
		st.setReachable(true)
	}
	if got := st.CurrentStep(); got != stepProPowerOff {
		t.Fatalf("step = %d, want %d — must not reach step 5 logic before step 3 completes", got, stepProPowerOff)
	}
}

// TestSetup_DescriptorIgnoredBeforeStep2 is advisor blocking #1: a descriptor fetch that
// arrives while the machine is still at step 1 must NOT latch and skip the reboot step.
func TestSetup_DescriptorIgnoredBeforeStep2(t *testing.T) {
	st, cfg, _, _ := newTestSetup(t)
	// TV probes the descriptor during ordinary discovery, before the Pro is paired.
	st.markTVDescriptorSeen()
	if st.TVDescriptorSeen() {
		t.Fatal("descriptor latched at step 1 — would skip the reboot step")
	}
	// Now pair the Pro: step 1 -> 2. The descriptor must still be required.
	pairPro(t, cfg)
	st.recomputeNow()
	if got := st.CurrentStep(); got != stepRebootTV {
		t.Fatalf("step = %d, want %d (reboot still required)", got, stepRebootTV)
	}
	// A real fetch now (at step 2) advances.
	st.markTVDescriptorSeen()
	if got := st.CurrentStep(); got != stepProPowerOff {
		t.Fatalf("step = %d, want %d after a step-2 descriptor fetch", got, stepProPowerOff)
	}
}

// TestSetup_LatchSurvivesRecompute checks the down-edge latch holds across later
// recomputes (a reachable report after the Pro was powered off must not un-advance step 3).
func TestSetup_LatchSurvivesRecompute(t *testing.T) {
	st, cfg, _, _ := newTestSetup(t)
	pairPro(t, cfg)
	st.recomputeNow()
	st.markTVDescriptorSeen()
	st.setReachable(true)
	st.setReachable(false) // step 3 -> 4
	if got := st.CurrentStep(); got != stepTVScan {
		t.Fatalf("step = %d, want %d", got, stepTVScan)
	}
	// A spurious reachable report (e.g. the Pro briefly answers) must not regress.
	st.setReachable(true)
	if got := st.CurrentStep(); got != stepTVScan {
		t.Fatalf("step regressed/advanced to %d on a spurious reachable; want %d", got, stepTVScan)
	}
}

func TestSetup_OnChangeFires(t *testing.T) {
	st, cfg, _, _ := newTestSetup(t)
	calls := 0
	st.setOnChange(func() { calls++ })
	pairPro(t, cfg)
	st.recomputeNow()
	if calls == 0 {
		t.Fatal("onChange not fired on a state change")
	}
}
