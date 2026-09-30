package leveling

import (
	"context"
	"testing"
	"time"

	"github.com/wyw14/cry-96/internal/gate"
	"github.com/wyw14/cry-96/internal/hydraulics"
	"github.com/wyw14/cry-96/internal/journal"
	"github.com/wyw14/cry-96/internal/model"
)

type fakeLookup struct {
	cycles map[string]model.TransitCycle
}

func (f *fakeLookup) Current(chamberID string) (model.TransitCycle, bool) {
	cycleValue, ok := f.cycles[chamberID]
	return cycleValue, ok
}

type harness struct {
	service   *Service
	lookup    *fakeLookup
	hydraulic *hydraulics.Controller
	now       time.Time
}

func newHarness(t *testing.T, cycleValue model.TransitCycle) *harness {
	t.Helper()
	base := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	now := func() time.Time { return base }
	recorder := journal.NewRecorder(journal.NewMemoryStore(), now)
	ramp := hydraulics.NewRamp(time.Millisecond, now)
	hydraulic := hydraulics.NewController(gate.NewCommands(), recorder, ramp, now)
	if err := hydraulic.BeginLeveling(cycleValue); err != nil {
		t.Fatalf("begin leveling: %v", err)
	}
	lookup := &fakeLookup{cycles: map[string]model.TransitCycle{"chamber-1": cycleValue}}
	service := NewService(lookup, hydraulic, recorder, now)
	service.Register("chamber-1")
	return &harness{service: service, lookup: lookup, hydraulic: hydraulic, now: base}
}

func (h *harness) observeFresh(t *testing.T, cycleValue model.TransitCycle, reach model.Reach, meters float64, age time.Duration) {
	t.Helper()
	s, err := model.NewReachLevelSample(cycleValue.ID, cycleValue.Generation, reach, meters, h.now.Add(-age))
	if err != nil {
		t.Fatalf("sample: %v", err)
	}
	if _, err := h.service.Observe(context.Background(), "chamber-1", s); err != nil {
		t.Fatalf("observe %s: %v", reach, err)
	}
}

func TestObserveRejectsInactiveGeneration(t *testing.T) {
	cycleValue := newCycleAt(t, model.DirectionUpstream, model.StageLeveling)
	h := newHarness(t, cycleValue)

	stale, err := model.NewReachLevelSample(cycleValue.ID, cycleValue.Generation+1, model.ReachChamber, 8.5, h.now)
	if err != nil {
		t.Fatalf("sample: %v", err)
	}
	if _, err := h.service.Observe(context.Background(), "chamber-1", stale); err == nil {
		t.Fatalf("sample from a newer/unknown generation must be rejected")
	}
}

func TestObserveRejectsWithoutActiveTransit(t *testing.T) {
	cycleValue := newCycleAt(t, model.DirectionUpstream, model.StageLeveling)
	h := newHarness(t, cycleValue)
	sample := sample(t, cycleValue, model.ReachChamber, 8.5, time.Second)
	if _, err := h.service.Observe(context.Background(), "chamber-other", sample); err == nil {
		t.Fatalf("observe without active transit must fail")
	}
}

func TestExecuteIssuesRampForFreshPlan(t *testing.T) {
	cycleValue := newCycleAt(t, model.DirectionUpstream, model.StageLeveling)
	h := newHarness(t, cycleValue)
	h.observeFresh(t, cycleValue, model.ReachUpstream, 10, time.Second)
	h.observeFresh(t, cycleValue, model.ReachChamber, 8.5, time.Second)
	h.observeFresh(t, cycleValue, model.ReachDownstream, 4, time.Second)

	plan, err := h.service.BuildPlan("chamber-1")
	if err != nil {
		t.Fatalf("build plan: %v", err)
	}
	if !plan.Executable {
		t.Fatalf("plan should be executable: %v", plan.BlockedReasons)
	}

	execution, err := h.service.Execute(context.Background(), "chamber-1")
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if execution.Ramp.Cancelled {
		t.Fatalf("ramp must not be cancelled")
	}
	if len(execution.Ramp.Steps) != len(plan.Instructions) {
		t.Fatalf("ramp steps %d != instructions %d", len(execution.Ramp.Steps), len(plan.Instructions))
	}
	if execution.Plan.CycleID != cycleValue.ID || execution.Plan.Generation != cycleValue.Generation {
		t.Fatalf("execution must be stamped with the current transit generation")
	}
}

func TestExecuteBlockedWhenSamplesGoStale(t *testing.T) {
	cycleValue := newCycleAt(t, model.DirectionUpstream, model.StageLeveling)
	h := newHarness(t, cycleValue)
	h.observeFresh(t, cycleValue, model.ReachUpstream, 10, 2*time.Second)
	h.observeFresh(t, cycleValue, model.ReachChamber, 8.5, 2*time.Second)
	h.observeFresh(t, cycleValue, model.ReachDownstream, 4, 2*time.Second)

	plan, err := h.service.BuildPlan("chamber-1")
	if err != nil {
		t.Fatalf("build plan: %v", err)
	}
	if !plan.Executable {
		t.Fatalf("plan should initially be executable: %v", plan.BlockedReasons)
	}

	// Advance the clock beyond freshness without new telemetry.
	future := h.now.Add(2 * time.Minute)
	h.service.now = func() time.Time { return future }

	if _, err := h.service.Execute(context.Background(), "chamber-1"); err == nil {
		t.Fatalf("execution must be refused once samples are no longer fresh")
	}
}

func TestOldGenerationPlanCanNeverExecute(t *testing.T) {
	oldCycle := newCycleAt(t, model.DirectionUpstream, model.StageLeveling)
	h := newHarness(t, oldCycle)
	h.observeFresh(t, oldCycle, model.ReachUpstream, 10, time.Second)
	h.observeFresh(t, oldCycle, model.ReachChamber, 8.5, time.Second)
	h.observeFresh(t, oldCycle, model.ReachDownstream, 4, time.Second)

	// A new transit generation takes over the chamber while the old,
	// still-displayed plan remains on screen.
	newCycle := oldCycle
	newCycle.Generation = oldCycle.Generation + 1
	if err := h.hydraulic.BeginLeveling(newCycle); err != nil {
		t.Fatalf("begin new leveling: %v", err)
	}
	h.lookup.cycles["chamber-1"] = newCycle

	if _, err := h.service.Execute(context.Background(), "chamber-1"); err == nil {
		t.Fatalf("old generation plan must not execute against the new transit")
	}

	// A rebuilt plan is blocked until fresh samples for the new generation arrive.
	plan, err := h.service.BuildPlan("chamber-1")
	if err != nil {
		t.Fatalf("build plan: %v", err)
	}
	if plan.Executable || plan.Generation != newCycle.Generation {
		t.Fatalf("new plan must carry the new generation and be blocked on old samples")
	}
}

func TestLatestSamplesAreGenerationScoped(t *testing.T) {
	cycleValue := newCycleAt(t, model.DirectionDownstream, model.StageLeveling)
	h := newHarness(t, cycleValue)
	h.observeFresh(t, cycleValue, model.ReachChamber, 5.5, time.Second)
	samples := h.service.LatestSamples("chamber-1")
	if len(samples) != 1 || samples[0].Reach != model.ReachChamber {
		t.Fatalf("expected one chamber reading, got %+v", samples)
	}
	if h.service.Config("chamber-1").TargetToleranceM <= 0 {
		t.Fatalf("registered chamber must expose default config")
	}
}
