package leveling

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/wyw14/cry-96/internal/model"
)

func newCycleAt(t *testing.T, direction model.Direction, stage model.TransitStage) model.TransitCycle {
	t.Helper()
	cycleValue, err := model.NewTransitCycle("chamber-1", uuid.New(), direction, 7, time.Date(2026, 9, 30, 10, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("cycle: %v", err)
	}
	now := cycleValue.CreatedAt
	transitions := []model.TransitStage{model.StageAdmitting, model.StageSealed, model.StageLeveling, model.StageReleasing}
	for _, next := range transitions {
		if cycleValue.Stage == stage {
			break
		}
		if err := cycleValue.Advance(next, "test", now); err != nil {
			t.Fatalf("advance: %v", err)
		}
		now = now.Add(time.Second)
	}
	if cycleValue.Stage != stage {
		t.Fatalf("could not reach stage %s, ended at %s", stage, cycleValue.Stage)
	}
	return cycleValue
}

func sample(t *testing.T, cycleValue model.TransitCycle, reach model.Reach, meters float64, age time.Duration) model.ReachLevelSample {
	t.Helper()
	s, err := model.NewReachLevelSample(cycleValue.ID, cycleValue.Generation, reach, meters,
		time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC).Add(-age))
	if err != nil {
		t.Fatalf("sample %s: %v", reach, err)
	}
	return s
}

func threeSamples(t *testing.T, cycleValue model.TransitCycle, upstreamM, chamberM, downstreamM float64, age time.Duration) map[model.Reach]model.ReachLevelSample {
	t.Helper()
	return map[model.Reach]model.ReachLevelSample{
		model.ReachUpstream:   sample(t, cycleValue, model.ReachUpstream, upstreamM, age),
		model.ReachChamber:    sample(t, cycleValue, model.ReachChamber, chamberM, age),
		model.ReachDownstream: sample(t, cycleValue, model.ReachDownstream, downstreamM, age),
	}
}

func TestBuildFillPlanTowardUpstream(t *testing.T) {
	cycleValue := newCycleAt(t, model.DirectionUpstream, model.StageLeveling)
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	samples := threeSamples(t, cycleValue, 10, 8.5, 4, 2*time.Second)

	plan := Build(DefaultConfig(), cycleValue, samples, now)

	if !plan.Executable {
		t.Fatalf("plan should be executable, blocked: %v", plan.BlockedReasons)
	}
	if plan.TargetReach != model.ReachUpstream || plan.TargetLevelM != 10 {
		t.Fatalf("target should be upstream pool at 10, got %s %.2f", plan.TargetReach, plan.TargetLevelM)
	}
	if plan.Mode != ModeFill || plan.GapM != 1.5 {
		t.Fatalf("expected fill gap 1.5, got mode %s gap %.2f", plan.Mode, plan.GapM)
	}
	// 1.5m / 0.5m coarse = 3 coarse steps + 1 fine trim + 1 close.
	if len(plan.Instructions) != 5 {
		t.Fatalf("expected 5 instructions, got %d: %+v", len(plan.Instructions), plan.Instructions)
	}
	for i := 0; i < 3; i++ {
		step := plan.Instructions[i]
		if step.Device != model.DeviceFillValve || step.SignedTarget != 0.8 {
			t.Fatalf("coarse step %d should open fill valve at +0.8, got %s %.2f", i, step.Device, step.SignedTarget)
		}
		if step.Rationale == "" {
			t.Fatalf("coarse step %d must explain itself", i)
		}
	}
	if plan.Instructions[3].SignedTarget != 0.25 || plan.Instructions[3].Device != model.DeviceFillValve {
		t.Fatalf("fine trim should open fill valve at +0.25, got %+v", plan.Instructions[3])
	}
	close := plan.Instructions[4]
	if close.SignedTarget != 0 || close.Device != model.DeviceFillValve {
		t.Fatalf("final step must close fill valve, got %+v", close)
	}
	if len(plan.Targets()) != 5 {
		t.Fatalf("targets must mirror instructions")
	}
}

func TestBuildDrainPlanTowardDownstream(t *testing.T) {
	cycleValue := newCycleAt(t, model.DirectionDownstream, model.StageLeveling)
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	samples := threeSamples(t, cycleValue, 9.9, 5.5, 4, 2*time.Second)

	plan := Build(DefaultConfig(), cycleValue, samples, now)

	if !plan.Executable {
		t.Fatalf("plan should be executable, blocked: %v", plan.BlockedReasons)
	}
	if plan.TargetReach != model.ReachDownstream || plan.Mode != ModeDrain || plan.GapM != -1.5 {
		t.Fatalf("expected drain toward downstream gap -1.5, got %s %s %.2f", plan.TargetReach, plan.Mode, plan.GapM)
	}
	for i := 0; i < 3; i++ {
		if plan.Instructions[i].Device != model.DeviceDrainValve || plan.Instructions[i].SignedTarget >= 0 {
			t.Fatalf("coarse step %d must open drain valve with negative target", i)
		}
	}
	if plan.Instructions[4].Device != model.DeviceDrainValve || plan.Instructions[4].SignedTarget != 0 {
		t.Fatalf("final step must close drain valve, got %+v", plan.Instructions[4])
	}
}

func TestBuildAlreadyLevelPlan(t *testing.T) {
	cycleValue := newCycleAt(t, model.DirectionUpstream, model.StageLeveling)
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	samples := threeSamples(t, cycleValue, 10.01, 10.0, 4, time.Second)

	plan := Build(DefaultConfig(), cycleValue, samples, now)

	if !plan.Executable || plan.Mode != ModeLevel {
		t.Fatalf("level plan should execute in level mode, blocked: %v", plan.BlockedReasons)
	}
	if len(plan.Instructions) != 1 || plan.Instructions[0].SignedTarget != 0 {
		t.Fatalf("level plan must be a single close instruction, got %+v", plan.Instructions)
	}
}

func TestBuildBlocksWithoutFreshSamples(t *testing.T) {
	cycleValue := newCycleAt(t, model.DirectionUpstream, model.StageLeveling)
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)

	t.Run("missing reach", func(t *testing.T) {
		plan := Build(DefaultConfig(), cycleValue, map[model.Reach]model.ReachLevelSample{
			model.ReachUpstream: sample(t, cycleValue, model.ReachUpstream, 10, time.Second),
			model.ReachChamber:  sample(t, cycleValue, model.ReachChamber, 8.5, time.Second),
		}, now)
		if plan.Executable {
			t.Fatalf("plan with missing sample must not be executable")
		}
		if !contains(plan.BlockedReasons, "missing level sample: downstream") {
			t.Fatalf("expected missing downstream reason, got %v", plan.BlockedReasons)
		}
	})

	t.Run("stale sample", func(t *testing.T) {
		stale := threeSamples(t, cycleValue, 10, 8.5, 4, 31*time.Second)
		plan := Build(DefaultConfig(), cycleValue, stale, now)
		if plan.Executable {
			t.Fatalf("stale plan must not be executable")
		}
		if !anyContains(plan.BlockedReasons, "stale level sample") {
			t.Fatalf("expected stale reason, got %v", plan.BlockedReasons)
		}
	})

	t.Run("wrong generation", func(t *testing.T) {
		samples := threeSamples(t, cycleValue, 10, 8.5, 4, time.Second)
		old := samples[model.ReachChamber]
		old.Generation = cycleValue.Generation - 1
		samples[model.ReachChamber] = old
		plan := Build(DefaultConfig(), cycleValue, samples, now)
		if plan.Executable {
			t.Fatalf("cross-generation plan must not be executable")
		}
		if !anyContains(plan.BlockedReasons, "generation") {
			t.Fatalf("expected generation mismatch reason, got %v", plan.BlockedReasons)
		}
	})

	t.Run("wrong stage", func(t *testing.T) {
		planned := newCycleAt(t, model.DirectionUpstream, model.StagePlanned)
		samples := map[model.Reach]model.ReachLevelSample{
			model.ReachUpstream:   sample(t, planned, model.ReachUpstream, 10, time.Second),
			model.ReachChamber:    sample(t, planned, model.ReachChamber, 8.5, time.Second),
			model.ReachDownstream: sample(t, planned, model.ReachDownstream, 4, time.Second),
		}
		plan := Build(DefaultConfig(), planned, samples, now)
		if plan.Executable {
			t.Fatalf("plan outside leveling stage must not execute")
		}
	})

	t.Run("implausible target pool", func(t *testing.T) {
		samples := threeSamples(t, cycleValue, 12.5, 8.5, 4, time.Second)
		plan := Build(DefaultConfig(), cycleValue, samples, now)
		if plan.Executable {
			t.Fatalf("plan driven by implausible pool telemetry must not execute")
		}
		if !anyContains(plan.BlockedReasons, "deviates") {
			t.Fatalf("expected pool drift reason, got %v", plan.BlockedReasons)
		}
	})
}

func TestInstructionsRespectMaxSteps(t *testing.T) {
	cycleValue := newCycleAt(t, model.DirectionUpstream, model.StageLeveling)
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	config := DefaultConfig()
	config.MaxSteps = 4 // 2 coarse + fine + close
	samples := threeSamples(t, cycleValue, 10, 4, 4, time.Second)

	plan := Build(config, cycleValue, samples, now)
	if !plan.Executable {
		t.Fatalf("blocked: %v", plan.BlockedReasons)
	}
	if len(plan.Instructions) != 4 {
		t.Fatalf("ramp must fit within max steps, got %d", len(plan.Instructions))
	}
	if plan.Instructions[3].SignedTarget != 0 {
		t.Fatalf("last step must always close")
	}
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func anyContains(values []string, fragment string) bool {
	for _, value := range values {
		if strings.Contains(value, fragment) {
			return true
		}
	}
	return false
}
