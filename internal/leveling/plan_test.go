package leveling

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/wyw14/cry-96/internal/cycle"
	"github.com/wyw14/cry-96/internal/gate"
	"github.com/wyw14/cry-96/internal/hydraulics"
	"github.com/wyw14/cry-96/internal/journal"
	"github.com/wyw14/cry-96/internal/model"
)

func levelCycle(t *testing.T, direction model.Direction, generation uint64, now time.Time) model.TransitCycle {
	t.Helper()
	cycleValue, err := model.NewTransitCycle("chamber-1", uuid.New(), direction, generation, now)
	if err != nil {
		t.Fatalf("cycle: %v", err)
	}
	for _, stage := range []model.TransitStage{model.StageAdmitting, model.StageSealed, model.StageLeveling} {
		if err := cycleValue.Advance(stage, "test", now); err != nil {
			t.Fatalf("advance: %v", err)
		}
	}
	return cycleValue
}

func freshSnapshot(cycleValue model.TransitCycle, upstreamM, downstreamM, chamberM float64, now time.Time) Snapshot {
	mk := func(role Role, meters float64) Reading {
		return Reading{ChamberID: cycleValue.ChamberID, Role: role, CycleID: cycleValue.ID, Generation: cycleValue.Generation, Meters: meters, ObservedAt: now}
	}
	return Snapshot{
		ChamberID: cycleValue.ChamberID,
		Readings: map[Role]Reading{
			RoleUpstream:   mk(RoleUpstream, upstreamM),
			RoleDownstream: mk(RoleDownstream, downstreamM),
			RoleChamber:    mk(RoleChamber, chamberM),
		},
		Fresh:    map[Role]bool{RoleUpstream: true, RoleDownstream: true, RoleChamber: true},
		HasAll:   true,
		AllFresh: true,
		Now:      now,
	}
}

func TestBuildPlanUpstreamFill(t *testing.T) {
	now := time.Date(2026, 9, 30, 10, 0, 0, 0, time.UTC)
	cycleValue := levelCycle(t, model.DirectionUpstream, 1, now)
	cfg := DefaultConfig()
	snap := freshSnapshot(cycleValue, 10, 2, 5, now)

	plan, err := BuildPlan(cycleValue, snap, cfg, now)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if plan.TargetRole != RoleUpstream || plan.TargetLevel != 10 {
		t.Fatalf("target = %s %.2f, want upstream 10", plan.TargetRole, plan.TargetLevel)
	}
	if plan.Mode != ModeFill {
		t.Fatalf("mode = %s, want fill", plan.Mode)
	}
	// distance 4.9 m with 1 m steps -> 5 bounded segments + explicit close.
	if len(plan.Steps) != 6 {
		t.Fatalf("steps = %d, want 6", len(plan.Steps))
	}
	for i, step := range plan.Steps[:5] {
		if step.Device != model.DeviceFillValve || step.SignedTarget <= 0 {
			t.Fatalf("step %d must drive the fill valve with a positive target: %+v", i, step)
		}
		if step.Opening > cfg.MaxOpeningPercent {
			t.Fatalf("step %d opening %.0f exceeds capability %.0f", i, step.Opening, cfg.MaxOpeningPercent)
		}
	}
	last := plan.Steps[len(plan.Steps)-1]
	if !last.ClosesValve || last.Opening != 0 {
		t.Fatalf("last step must close the valve: %+v", last)
	}
	if plan.Steps[4].Opening != cfg.FinalOpeningPercent {
		t.Fatalf("final approach opening = %.0f, want creep %.0f", plan.Steps[4].Opening, cfg.FinalOpeningPercent)
	}
	if len(plan.Reasons) == 0 {
		t.Fatalf("plan must carry an explanation")
	}
}

func TestBuildPlanDownstreamDrain(t *testing.T) {
	now := time.Now().UTC()
	cycleValue := levelCycle(t, model.DirectionDownstream, 1, now)
	snap := freshSnapshot(cycleValue, 12, 2, 8, now)

	plan, err := BuildPlan(cycleValue, snap, DefaultConfig(), now)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if plan.TargetRole != RoleDownstream || plan.Mode != ModeDrain {
		t.Fatalf("target/mode = %s/%s, want downstream/drain", plan.TargetRole, plan.Mode)
	}
	for _, step := range plan.Steps {
		if step.Device != model.DeviceDrainValve || (!step.ClosesValve && step.SignedTarget >= 0) {
			t.Fatalf("drain step must target the drain valve negatively: %+v", step)
		}
	}
}

func TestBuildPlanAlreadyLevel(t *testing.T) {
	now := time.Now().UTC()
	cycleValue := levelCycle(t, model.DirectionUpstream, 1, now)
	snap := freshSnapshot(cycleValue, 5.05, 2, 5.0, now) // 0.05 head within 0.10
	plan, err := BuildPlan(cycleValue, snap, DefaultConfig(), now)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if plan.Mode != ModeLevel || len(plan.Steps) != 0 {
		t.Fatalf("want a level no-op plan, got mode=%s steps=%d", plan.Mode, len(plan.Steps))
	}
}

func TestBuildPlanRejectsStaleAndForeignSamples(t *testing.T) {
	now := time.Now().UTC()
	cycleValue := levelCycle(t, model.DirectionUpstream, 1, now)

	stale := freshSnapshot(cycleValue, 10, 2, 5, now)
	stale.AllFresh = false
	if _, err := BuildPlan(cycleValue, stale, DefaultConfig(), now); err == nil {
		t.Fatal("expected refusal when a sample is not fresh")
	}

	missing := freshSnapshot(cycleValue, 10, 2, 5, now)
	missing.HasAll = false
	if _, err := BuildPlan(cycleValue, missing, DefaultConfig(), now); err == nil {
		t.Fatal("expected refusal when a sample role is missing")
	}

	foreign := freshSnapshot(cycleValue, 10, 2, 5, now)
	readings := foreign.Readings[RoleChamber]
	readings.Generation = cycleValue.Generation + 1
	foreign.Readings[RoleChamber] = readings
	if _, err := BuildPlan(cycleValue, foreign, DefaultConfig(), now); err == nil {
		t.Fatal("expected refusal when a sample belongs to another generation")
	}

	planned := cycleValue
	if err := planned.Advance(model.StageReleasing, "test", now); err == nil {
		// Leveling -> Releasing is legal; planning there must be rejected.
		if _, err := BuildPlan(planned, freshSnapshot(planned, 10, 2, 5, now), DefaultConfig(), now); err == nil {
			t.Fatal("expected refusal outside the leveling stage")
		}
	}
}

type coordinatorHandle struct {
	cycles   *cycle.Coordinator
	recorder *journal.Recorder
	ctx      context.Context
}

type clock struct{ value time.Time }

func (c *clock) get() time.Time { return c.value }

func newServiceHarness(t *testing.T) (*Service, *coordinatorHandle, *clock) {
	t.Helper()
	clk := &clock{value: time.Date(2026, 9, 30, 10, 0, 0, 0, time.UTC)}
	store := journal.NewMemoryStore()
	recorder := journal.NewRecorder(store, clk.get)
	commands := gate.NewCommands()
	gates := gate.NewController(gate.NewState("chamber-1"), commands, recorder, clk.get)
	ramp := hydraulics.NewRamp(time.Millisecond, clk.get)
	hydraulicsController := hydraulics.NewController(commands, recorder, ramp, clk.get)
	cycles := cycle.NewCoordinator(gates, hydraulicsController, recorder, clk.get)
	readings := NewReadingStore(clk.get)
	svc := NewService(cycles, hydraulicsController, readings, recorder, clk.get)
	if _, err := svc.Configure("chamber-1", DefaultConfig()); err != nil {
		t.Fatalf("configure: %v", err)
	}
	return svc, &coordinatorHandle{cycles: cycles, recorder: recorder, ctx: context.Background()}, clk
}

func seedCycle(t *testing.T, h *coordinatorHandle, direction model.Direction, generation uint64, clk *clock) model.TransitCycle {
	t.Helper()
	cycleValue := levelCycle(t, direction, generation, clk.get())
	h.cycles.Restore(cycleValue)
	return cycleValue
}

func feedAll(t *testing.T, svc *Service, upstreamM, downstreamM, chamberM float64) {
	t.Helper()
	for _, value := range []struct {
		role   Role
		meters float64
	}{
		{RoleUpstream, upstreamM}, {RoleDownstream, downstreamM}, {RoleChamber, chamberM},
	} {
		if _, err := svc.Observe("chamber-1", value.role, value.meters, time.Time{}); err != nil {
			t.Fatalf("observe %s: %v", value.role, err)
		}
	}
}

func TestServicePlanAndExecute(t *testing.T) {
	svc, h, clk := newServiceHarness(t)
	seedCycle(t, h, model.DirectionUpstream, 1, clk)
	feedAll(t, svc, 10, 2, 5)

	plan, err := svc.Plan(h.ctx, "chamber-1")
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if plan.Generation != 1 || plan.Status != StatusPlanned {
		t.Fatalf("unexpected plan: gen=%d status=%s", plan.Generation, plan.Status)
	}

	executed, err := svc.Execute(h.ctx, "chamber-1")
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if executed.Status != StatusExecuted || executed.ExecutedAt == nil {
		t.Fatalf("plan not executed: %+v", executed)
	}
	for i, step := range executed.Steps {
		if step.CommandID == nil {
			t.Fatalf("step %d was not correlated to an issued command", i)
		}
	}

	// An executed plan must not run again.
	if _, err := svc.Execute(h.ctx, "chamber-1"); err == nil {
		t.Fatal("executed plan must not be re-run")
	}
}

func TestServiceRefusesPlanWithoutFreshSamples(t *testing.T) {
	svc, h, clk := newServiceHarness(t)
	seedCycle(t, h, model.DirectionUpstream, 1, clk)
	feedAll(t, svc, 10, 2, 5)

	clk.value = clk.value.Add(DefaultConfig().Freshness + time.Second)
	if _, err := svc.Plan(h.ctx, "chamber-1"); err == nil {
		t.Fatal("expected plan refusal once samples are stale")
	}
}

func TestServiceFencesOldGeneration(t *testing.T) {
	svc, h, clk := newServiceHarness(t)
	seedCycle(t, h, model.DirectionUpstream, 1, clk)
	feedAll(t, svc, 10, 2, 5)
	if _, err := svc.Plan(h.ctx, "chamber-1"); err != nil {
		t.Fatalf("plan: %v", err)
	}

	// A new transit generation starts (e.g. recovery or a new convoy) while the
	// old plan is still sitting there.
	seedCycle(t, h, model.DirectionUpstream, 2, clk)
	if _, err := svc.Execute(h.ctx, "chamber-1"); err == nil {
		t.Fatal("old generation plan must not execute against the new cycle")
	}
	plan, ok := svc.CurrentPlan("chamber-1")
	if !ok || plan.Status != StatusFailed {
		t.Fatalf("old plan should be marked failed, got %+v", plan)
	}
}

func TestServiceSupersedesReplannedTarget(t *testing.T) {
	svc, h, clk := newServiceHarness(t)
	seedCycle(t, h, model.DirectionUpstream, 1, clk)
	feedAll(t, svc, 10, 2, 5)
	first, err := svc.Plan(h.ctx, "chamber-1")
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	feedAll(t, svc, 9.5, 2, 5)
	second, err := svc.Plan(h.ctx, "chamber-1")
	if err != nil {
		t.Fatalf("replan: %v", err)
	}
	if first.ID == second.ID {
		t.Fatal("replanning must store a new plan")
	}
	if second.TargetLevel != 9.5 || second.Status != StatusPlanned {
		t.Fatalf("replanned target stale: %.2f status=%s", second.TargetLevel, second.Status)
	}
}
