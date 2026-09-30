package leveling

import (
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/google/uuid"
	"github.com/wyw14/cry-96/internal/model"
)

// Mode describes which way the chamber must move.
type Mode string

const (
	ModeFill  Mode = "fill"
	ModeDrain Mode = "drain"
	ModeLevel Mode = "level"
)

// PlanStatus tracks whether a plan may still be driven to the valves.
type PlanStatus string

const (
	StatusPlanned    PlanStatus = "planned"
	StatusExecuted   PlanStatus = "executed"
	StatusFailed     PlanStatus = "failed"
	StatusSuperseded PlanStatus = "superseded"
)

// ValveStep is one explainable entry in the stepwise valve sequence.
type ValveStep struct {
	Index        int              `json:"index"`
	Device       model.DeviceKind `json:"device"`
	Action       string           `json:"action"`
	Opening      float64          `json:"opening"`
	SignedTarget float64          `json:"signed_target"`
	FromLevel    float64          `json:"from_level"`
	ToLevel      float64          `json:"to_level"`
	Rationale    string           `json:"rationale"`
	ClosesValve  bool             `json:"closes_valve"`
	CommandID    *uuid.UUID       `json:"command_id,omitempty"`
}

// Basis records the exact readings a plan was derived from, so an operator can
// see why a target was chosen and tell stale data apart from the decision.
type Basis struct {
	Target          Reading   `json:"target"`
	Chamber         Reading   `json:"chamber"`
	Upstream        Reading   `json:"upstream"`
	Downstream      Reading   `json:"downstream"`
	TargetObserved  time.Time `json:"target_observed_at"`
	ChamberObserved time.Time `json:"chamber_observed_at"`
}

// Plan is the explainable leveling target for one transit generation.
type Plan struct {
	ID           uuid.UUID       `json:"id"`
	ChamberID    string          `json:"chamber_id"`
	CycleID      uuid.UUID       `json:"cycle_id"`
	Generation   uint64          `json:"generation"`
	Direction    model.Direction `json:"direction"`
	TargetRole   Role            `json:"target_role"`
	TargetLevel  float64         `json:"target_level"`
	ChamberLevel float64         `json:"chamber_level"`
	ToleranceM   float64         `json:"tolerance_m"`
	HeadM        float64         `json:"head_m"`
	Mode         Mode            `json:"mode"`
	Steps        []ValveStep     `json:"steps"`
	Reasons      []string        `json:"reasons"`
	Basis        Basis           `json:"basis"`
	Status       PlanStatus      `json:"status"`
	CreatedAt    time.Time       `json:"created_at"`
	ExecutedAt   *time.Time      `json:"executed_at,omitempty"`
	FailureNote  string          `json:"failure_note,omitempty"`
}

func (p *Plan) matches(cycleID uuid.UUID, generation uint64) bool {
	return p.CycleID == cycleID && p.Generation == generation
}

// BuildPlan derives the target level and the stepwise valve sequence purely
// from the current cycle identity, fresh readings and chamber parameters.
// It must not be called unless all three readings are present and fresh.
func BuildPlan(cycle model.TransitCycle, snap Snapshot, cfg ChamberConfig, now time.Time) (Plan, error) {
	if cycle.ChamberID == "" || cycle.ID == uuid.Nil || cycle.Generation == 0 {
		return Plan{}, errors.New("plan requires a current transit generation")
	}
	if cycle.Stage != model.StageLeveling {
		return Plan{}, fmt.Errorf("leveling target requires leveling stage, cycle is %s", cycle.Stage)
	}
	cfg = cfg.normalized()
	if err := cfg.validate(); err != nil {
		return Plan{}, err
	}
	if !snap.HasAll || !snap.AllFresh {
		return Plan{}, errors.New("refusing to plan: upstream, downstream and chamber samples must be present and fresh")
	}

	upstream := snap.Readings[RoleUpstream]
	downstream := snap.Readings[RoleDownstream]
	chamber := snap.Readings[RoleChamber]
	for _, reading := range []Reading{upstream, downstream, chamber} {
		if reading.CycleID != cycle.ID || reading.Generation != cycle.Generation {
			return Plan{}, errors.New("refusing to plan: a sample belongs to another transit generation")
		}
	}

	// A transit entering from upstream starts at the downstream pool and must
	// rise to the upstream pool; a downstream transit is the mirror image.
	targetRole := RoleUpstream
	if cycle.Direction == model.DirectionDownstream {
		targetRole = RoleDownstream
	}
	target := upstream
	if targetRole == RoleDownstream {
		target = downstream
	}

	head := target.Meters - chamber.Meters
	mode := ModeLevel
	if head > cfg.ToleranceM {
		mode = ModeFill
	} else if head < -cfg.ToleranceM {
		mode = ModeDrain
	}

	plan := Plan{
		ID: uuid.New(), ChamberID: cycle.ChamberID, CycleID: cycle.ID,
		Generation: cycle.Generation, Direction: cycle.Direction,
		TargetRole: targetRole, TargetLevel: target.Meters,
		ChamberLevel: chamber.Meters, ToleranceM: cfg.ToleranceM,
		HeadM: roundMeters(head), Mode: mode,
		Basis: Basis{
			Target: target, Chamber: chamber, Upstream: upstream, Downstream: downstream,
			TargetObserved: target.ObservedAt, ChamberObserved: chamber.ObservedAt,
		},
		Status: StatusPlanned, CreatedAt: now.UTC(),
	}
	plan.Reasons = explain(plan)
	plan.Steps = buildSteps(plan, cfg)
	return plan, nil
}

func explain(p Plan) []string {
	reasons := []string{
		fmt.Sprintf("transit runs %s: the exit gate opens onto the %s pool, so that pool is the target", p.Direction, p.TargetRole),
		fmt.Sprintf("target level %.2f m read from %s sample at %s", p.TargetLevel, p.TargetRole, p.Basis.TargetObserved.Format(time.RFC3339)),
		fmt.Sprintf("chamber level %.2f m read at %s", p.ChamberLevel, p.Basis.ChamberObserved.Format(time.RFC3339)),
	}
	switch p.Mode {
	case ModeFill:
		reasons = append(reasons, fmt.Sprintf("head is +%.2f m (above %.2f m tolerance): open the fill valve and rise toward target", p.HeadM, p.ToleranceM))
	case ModeDrain:
		reasons = append(reasons, fmt.Sprintf("head is %.2f m (below -%.2f m tolerance): open the drain valve and fall toward target", p.HeadM, p.ToleranceM))
	default:
		reasons = append(reasons, fmt.Sprintf("|head| %.2f m is within %.2f m tolerance: chamber is level, no valve movement required", math.Abs(p.HeadM), p.ToleranceM))
	}
	return reasons
}

// buildSteps turns the required elevation change into bounded opening steps
// followed by an explicit valve close. Openings stay within the chamber's
// valve capability and the final approach creeps in at the small opening.
func buildSteps(p Plan, cfg ChamberConfig) []ValveStep {
	if p.Mode == ModeLevel {
		return nil
	}
	device := model.DeviceFillValve
	sign := 1.0
	if p.Mode == ModeDrain {
		device = model.DeviceDrainValve
		sign = -1.0
	}

	distance := math.Abs(p.HeadM) - cfg.ToleranceM
	if distance <= 0 {
		return nil
	}
	segments := int(math.Ceil(distance/cfg.MaxStepM - 1e-9))
	if segments < 1 {
		segments = 1
	}

	steps := make([]ValveStep, 0, segments+1)
	level := p.ChamberLevel
	covered := 0.0
	for i := 0; i < segments; i++ {
		chunk := math.Min(cfg.MaxStepM, distance-covered)
		from := level
		level = level + sign*chunk
		// Stay just inside the tolerance band rather than overshooting target.
		if i == segments-1 {
			level = p.TargetLevel - sign*cfg.ToleranceM
		}
		opening := cfg.MaxOpeningPercent
		rationale := fmt.Sprintf("segment %d/%d: transfer up to %.2f m (%.2f -> %.2f m) at bounded opening %.0f%%", i+1, segments, chunk, roundMeters(from), roundMeters(level), opening)
		if i == segments-1 {
			opening = cfg.FinalOpeningPercent
			rationale = fmt.Sprintf("final approach %d/%d: creep the last %.2f m into the %.2f m band at opening %.0f%%", i+1, segments, chunk, cfg.ToleranceM, opening)
		}
		steps = append(steps, ValveStep{
			Index: i, Device: device, Action: "set_opening", Opening: opening,
			SignedTarget: sign * opening, FromLevel: roundMeters(from), ToLevel: roundMeters(level),
			Rationale: rationale,
		})
		covered += chunk
	}
	closeLevel := p.TargetLevel - sign*p.ToleranceM
	steps = append(steps, ValveStep{
		Index: len(steps), Device: device, Action: "set_opening", Opening: 0,
		SignedTarget: 0, FromLevel: roundMeters(closeLevel), ToLevel: roundMeters(closeLevel),
		Rationale: "close the valve once the chamber is inside the target band", ClosesValve: true,
	})
	return steps
}

func roundMeters(value float64) float64 {
	return math.Round(value*100) / 100
}
