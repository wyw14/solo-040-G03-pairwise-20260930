package leveling

import (
	"fmt"
	"math"
	"time"

	"github.com/google/uuid"
	"github.com/wyw14/cry-96/internal/model"
)

// Mode describes the flow direction the plan relies on.
type Mode string

const (
	ModeFill  Mode = "fill"
	ModeDrain Mode = "drain"
	ModeLevel Mode = "level"
)

// ValveInstruction is one explainable step of the staged valve ramp.
// SignedTarget follows the hydraulics ramp convention: positive opens the
// fill valve, negative opens the drain valve, zero closes.
type ValveInstruction struct {
	Step         int              `json:"step"`
	Device       model.DeviceKind `json:"device"`
	Action       string           `json:"action"`
	Opening      float64          `json:"opening"`
	SignedTarget float64          `json:"signed_target"`
	Rationale    string           `json:"rationale"`
}

// Plan is the explainable leveling target for one transit generation. It is
// only executable when every reach sample is present, fresh and stamped with
// the current generation.
type Plan struct {
	ChamberID      string                   `json:"chamber_id"`
	CycleID        uuid.UUID                `json:"cycle_id"`
	Generation     uint64                   `json:"generation"`
	Direction      model.Direction          `json:"direction"`
	Stage          model.TransitStage       `json:"stage"`
	TargetReach    model.Reach              `json:"target_reach"`
	TargetLevelM   float64                  `json:"target_level_m"`
	ChamberLevelM  float64                  `json:"chamber_level_m"`
	GapM           float64                  `json:"gap_m"`
	ToleranceM     float64                  `json:"tolerance_m"`
	Mode           Mode                     `json:"mode"`
	Executable     bool                     `json:"executable"`
	BlockedReasons []string                 `json:"blocked_reasons"`
	Samples        []model.ReachLevelSample `json:"samples"`
	SampleMaxAgeS  float64                  `json:"sample_max_age_seconds"`
	SampleAgeS     map[string]float64       `json:"sample_age_seconds"`
	Instructions   []ValveInstruction       `json:"instructions"`
	PlannedAt      time.Time                `json:"planned_at"`
}

// Targets returns the signed valve openings in ramp execution order.
func (p Plan) Targets() []float64 {
	targets := make([]float64, 0, len(p.Instructions))
	for _, instruction := range p.Instructions {
		targets = append(targets, instruction.SignedTarget)
	}
	return targets
}

// Build derives the leveling plan from chamber parameters, the current
// transit and the newest sample of each reach. It never mutates state.
func Build(config ChamberConfig, cycle model.TransitCycle, latest map[model.Reach]model.ReachLevelSample, now time.Time) Plan {
	now = now.UTC()
	plan := Plan{
		ChamberID:      cycle.ChamberID,
		CycleID:        cycle.ID,
		Generation:     cycle.Generation,
		Direction:      cycle.Direction,
		Stage:          cycle.Stage,
		ToleranceM:     config.TargetToleranceM,
		Mode:           ModeLevel,
		BlockedReasons: []string{},
		Samples:        []model.ReachLevelSample{},
		SampleMaxAgeS:  config.SampleFreshness.Seconds(),
		SampleAgeS:     map[string]float64{},
		Instructions:   []ValveInstruction{},
		PlannedAt:      now,
	}
	if cycle.Stage != model.StageLeveling {
		plan.BlockedReasons = append(plan.BlockedReasons,
			fmt.Sprintf("transit stage %q cannot accept leveling instructions", cycle.Stage))
	}

	targetReach := reachFor(cycle.Direction)
	plan.TargetReach = targetReach

	order := []model.Reach{model.ReachUpstream, model.ReachChamber, model.ReachDownstream}
	readings := map[model.Reach]model.ReachLevelSample{}
	for _, reach := range order {
		sample, ok := latest[reach]
		if !ok {
			plan.BlockedReasons = append(plan.BlockedReasons, fmt.Sprintf("missing level sample: %s", reach))
			continue
		}
		if sample.CycleID != cycle.ID || sample.Generation != cycle.Generation {
			plan.BlockedReasons = append(plan.BlockedReasons, fmt.Sprintf(
				"sample %s belongs to transit %s generation %d, current is %s generation %d",
				reach, sample.CycleID.String()[:8], sample.Generation, cycle.ID.String()[:8], cycle.Generation))
			continue
		}
		age := now.Sub(sample.ObservedAt)
		plan.SampleAgeS[string(reach)] = age.Seconds()
		plan.Samples = append(plan.Samples, sample)
		switch {
		case age < 0:
			plan.BlockedReasons = append(plan.BlockedReasons,
				fmt.Sprintf("sample %s timestamp is in the future", reach))
		case age > config.SampleFreshness:
			plan.BlockedReasons = append(plan.BlockedReasons, fmt.Sprintf(
				"stale level sample: %s age %.0fs exceeds freshness %.0fs",
				reach, age.Seconds(), config.SampleFreshness.Seconds()))
		}
		readings[reach] = sample
	}

	chamber, chamberOK := readings[model.ReachChamber]
	target, targetOK := readings[targetReach]
	if chamberOK {
		plan.ChamberLevelM = chamber.Meters
	}
	if targetOK {
		plan.TargetLevelM = target.Meters
		normal := config.UpstreamNormalM
		if targetReach == model.ReachDownstream {
			normal = config.DownstreamNormalM
		}
		if drift := math.Abs(target.Meters - normal); drift > config.PoolDriftM {
			plan.BlockedReasons = append(plan.BlockedReasons, fmt.Sprintf(
				"%s pool %.2fm deviates %.2fm from configured normal %.2fm (limit %.2fm)",
				targetReach, target.Meters, drift, normal, config.PoolDriftM))
		}
	}
	if chamberOK && targetOK {
		plan.GapM = target.Meters - chamber.Meters
	}

	if len(plan.BlockedReasons) > 0 {
		return plan
	}

	plan.Instructions, plan.Mode = instructions(config, plan)
	plan.Executable = true
	return plan
}

func reachFor(direction model.Direction) model.Reach {
	if direction == model.DirectionDownstream {
		return model.ReachDownstream
	}
	return model.ReachUpstream
}

// instructions stages bulk coarse flow, one fine trim step and a guaranteed
// valve close, and always fits within config.MaxSteps.
func instructions(config ChamberConfig, plan Plan) ([]ValveInstruction, Mode) {
	gap := plan.GapM
	if math.Abs(gap) <= config.TargetToleranceM {
		return []ValveInstruction{closeInstruction(1, plan.TargetReach,
				fmt.Sprintf("chamber %.2fm already within ±%.2fm of %s pool %.2fm",
					plan.ChamberLevelM, config.TargetToleranceM, plan.TargetReach, plan.TargetLevelM))},
			ModeLevel
	}

	mode := ModeFill
	device := model.DeviceFillValve
	if gap < 0 {
		mode = ModeDrain
		device = model.DeviceDrainValve
	}
	magnitude := math.Abs(gap)
	coarseNeeded := int(math.Ceil((magnitude - config.TargetToleranceM) / config.CoarseDeltaM))
	if coarseNeeded < 1 {
		coarseNeeded = 1
	}
	// Reserve the fine trim and close steps.
	if coarseNeeded > config.MaxSteps-2 {
		coarseNeeded = config.MaxSteps - 2
	}
	steps := make([]ValveInstruction, 0, coarseNeeded+2)
	for i := 1; i <= coarseNeeded; i++ {
		residual := math.Max(0, magnitude-float64(i)*config.CoarseDeltaM)
		steps = append(steps, ValveInstruction{
			Step:         i,
			Device:       device,
			Action:       "set_opening",
			Opening:      config.CoarseOpening,
			SignedTarget: signed(mode, config.CoarseOpening),
			Rationale: fmt.Sprintf("coarse %s step %d/%d at %.0f%% opening; sized for %.2fm per step, ~%.2fm residual before trim",
				mode, i, coarseNeeded, config.CoarseOpening*100, config.CoarseDeltaM, residual),
		})
	}

	steps = append(steps, ValveInstruction{
		Step:         coarseNeeded + 1,
		Device:       device,
		Action:       "set_opening",
		Opening:      config.FineOpening,
		SignedTarget: signed(mode, config.FineOpening),
		Rationale: fmt.Sprintf("fine %s trim at %.0f%% opening to approach %s pool %.2fm within ±%.2fm",
			mode, config.FineOpening*100, plan.TargetReach, plan.TargetLevelM, config.TargetToleranceM),
	})

	steps = append(steps, closeInstruction(coarseNeeded+2, plan.TargetReach, fmt.Sprintf(
		"close %s valve once chamber reaches %.2fm ±%.2fm", device, plan.TargetLevelM, config.TargetToleranceM)))
	return steps, mode
}

func closeInstruction(step int, targetReach model.Reach, rationale string) ValveInstruction {
	device := model.DeviceFillValve
	if targetReach == model.ReachDownstream {
		device = model.DeviceDrainValve
	}
	return ValveInstruction{
		Step: step, Device: device, Action: "set_opening",
		Opening: 0, SignedTarget: 0, Rationale: rationale,
	}
}

func signed(mode Mode, opening float64) float64 {
	if mode == ModeDrain {
		return -opening
	}
	return opening
}
