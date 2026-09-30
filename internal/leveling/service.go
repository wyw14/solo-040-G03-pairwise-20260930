package leveling

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/wyw14/cry-96/internal/cycle"
	"github.com/wyw14/cry-96/internal/hydraulics"
	"github.com/wyw14/cry-96/internal/journal"
	"github.com/wyw14/cry-96/internal/model"
)

// ValveRunner is the valve capability the leveling service drives. It is the
// existing hydraulics ramp, narrowed to the surface the module needs so that
// every issued step can be fenced against the current transit generation.
type ValveRunner interface {
	RampWith(ctx context.Context, cycleValue model.TransitCycle, targets []float64, issue hydraulics.StepIssuer) (hydraulics.RampResult, error)
	IssueValveCommand(ctx context.Context, cycleValue model.TransitCycle, target float64) (model.DeviceCommand, error)
	CloseValves(ctx context.Context, cycleValue model.TransitCycle) error
}

// Service owns per-chamber leveling parameters, fresh readings and the single
// plan of the current transit generation.
type Service struct {
	mu       sync.Mutex
	configs  map[string]ChamberConfig
	plans    map[string]Plan
	readings *ReadingStore
	cycles   *cycle.Coordinator
	valves   ValveRunner
	recorder *journal.Recorder
	now      func() time.Time
}

func NewService(cycles *cycle.Coordinator, valves ValveRunner, readings *ReadingStore, recorder *journal.Recorder, now func() time.Time) *Service {
	if now == nil {
		now = time.Now
	}
	return &Service{
		configs: make(map[string]ChamberConfig), plans: make(map[string]Plan),
		readings: readings, cycles: cycles, valves: valves, recorder: recorder, now: now,
	}
}

// Configure sets the leveling parameters for one chamber.
func (s *Service) Configure(chamberID string, cfg ChamberConfig) (ChamberConfig, error) {
	if chamberID == "" {
		return ChamberConfig{}, errors.New("chamber id is required")
	}
	cfg = cfg.normalized()
	if err := cfg.validate(); err != nil {
		return ChamberConfig{}, err
	}
	s.mu.Lock()
	s.configs[chamberID] = cfg
	s.mu.Unlock()
	return cfg, nil
}

// Config returns the effective parameters for a chamber.
func (s *Service) Config(chamberID string) ChamberConfig {
	s.mu.Lock()
	cfg, ok := s.configs[chamberID]
	s.mu.Unlock()
	if !ok {
		return DefaultConfig()
	}
	return cfg
}

// Observe records a real-time level reading for the current transit generation.
// Readings for a non-current generation are rejected upstream by the caller
// supplying the cycle identity; the store keeps them partitioned per cycle.
func (s *Service) Observe(chamberID string, role Role, meters float64, observedAt time.Time) (Reading, error) {
	cycleValue, err := s.activeCycle(chamberID)
	if err != nil {
		return Reading{}, err
	}
	return s.readings.Observe(cycleValue.ID, cycleValue.Generation, chamberID, role, meters, observedAt)
}

// Plan builds the explainable target for the current leveling generation. It
// refuses to produce an executable plan unless all three readings are present
// and fresh, and supersedes any earlier plan still awaiting execution.
func (s *Service) Plan(ctx context.Context, chamberID string) (Plan, error) {
	cycleValue, err := s.levelingCycle(chamberID)
	if err != nil {
		return Plan{}, err
	}
	cfg := s.Config(chamberID)
	snapshot := s.readings.SnapshotFor(chamberID, cycleValue.ID, cycleValue.Generation, cfg)
	plan, err := BuildPlan(cycleValue, snapshot, cfg, s.now())
	if err != nil {
		return Plan{}, err
	}
	s.mu.Lock()
	if previous, ok := s.plans[chamberID]; ok && previous.Status == StatusPlanned {
		previous.Status = StatusSuperseded
		previous.FailureNote = "superseded by a freshly planned target"
		s.plans[chamberID] = previous
	}
	s.plans[chamberID] = plan
	s.mu.Unlock()
	if _, err := s.recorder.Record(ctx, chamberID, model.EventLevelPlanCreated, plan.CycleID, plan.Generation, plan); err != nil {
		return Plan{}, err
	}
	return plan, nil
}

// Execute drives a planned, still-current sequence through the valve ramp. The
// plan is re-validated against the live cycle generation and sample freshness
// before the first step and before every later step; losing either aborts and
// closes the valves. A plan from an old generation can never be replayed.
func (s *Service) Execute(ctx context.Context, chamberID string) (Plan, error) {
	cycleValue, err := s.levelingCycle(chamberID)
	if err != nil {
		return Plan{}, err
	}
	s.mu.Lock()
	plan, ok := s.plans[chamberID]
	s.mu.Unlock()
	if !ok {
		return Plan{}, errors.New("no leveling plan for this chamber; plan before execute")
	}
	if !plan.matches(cycleValue.ID, cycleValue.Generation) {
		s.failPlan(chamberID, "plan belongs to an old transit generation; refusing to execute")
		return Plan{}, errors.New("refusing to execute: plan belongs to an old transit generation")
	}
	if plan.Status != StatusPlanned {
		return Plan{}, fmt.Errorf("plan is %s and cannot be executed again; plan a new target", plan.Status)
	}
	if err := s.ensureFresh(plan, cycleValue); err != nil {
		return Plan{}, err
	}
	if len(plan.Steps) == 0 {
		// Already level: record the no-op plan as executed without touching valves.
		return s.finish(ctx, chamberID, plan, nil)
	}

	targets := make([]float64, 0, len(plan.Steps))
	for _, step := range plan.Steps {
		targets = append(targets, step.SignedTarget)
	}
	issued := 0
	// The guard re-reads the authoritative coordinator before every step so a
	// generation that turns over mid-ramp is detected immediately rather than
	// compared against the cycle snapshot captured when Execute began.
	guard := func(runCtx context.Context, _ model.TransitCycle, signed float64) error {
		live, ok := s.cycles.Current(plan.ChamberID)
		if !ok || !live.IdentityMatches(plan.CycleID, plan.Generation) {
			return errors.New("current transit no longer matches the planned generation")
		}
		if err := s.ensureFresh(plan, live); err != nil {
			return err
		}
		command, err := s.valves.IssueValveCommand(runCtx, live, signed)
		if err != nil {
			return err
		}
		if issued < len(plan.Steps) {
			id := command.ID
			plan.Steps[issued].CommandID = &id
		}
		issued++
		return nil
	}

	result, rampErr := s.valves.RampWith(ctx, cycleValue, targets, guard)
	if rampErr != nil {
		failure := rampErr
		if result.Cancelled {
			failure = errors.New("leveling ramp was cancelled before completion")
		}
		// Best effort: leave the physical valves closed even when fencing
		// aborted midway, closing against whatever generation is now current.
		var closeErr error
		if live, ok := s.cycles.Current(plan.ChamberID); ok {
			closeErr = s.valves.CloseValves(ctx, live)
		}
		note := failure.Error()
		if closeErr != nil {
			note += "; valve close after abort also failed: " + closeErr.Error()
		}
		plan = s.failPlan(chamberID, note)
		_, _ = s.recorder.Record(ctx, chamberID, model.EventLevelPlanExecuted, plan.CycleID, plan.Generation, plan)
		return plan, failure
	}
	return s.finish(ctx, chamberID, plan, &result)
}

func (s *Service) finish(ctx context.Context, chamberID string, plan Plan, result *hydraulics.RampResult) (Plan, error) {
	executedAt := s.now().UTC()
	plan.Status = StatusExecuted
	plan.ExecutedAt = &executedAt
	s.mu.Lock()
	s.plans[chamberID] = plan
	s.mu.Unlock()
	if _, err := s.recorder.Record(ctx, chamberID, model.EventLevelPlanExecuted, plan.CycleID, plan.Generation, plan); err != nil {
		return Plan{}, err
	}
	return plan, nil
}

func (s *Service) failPlan(chamberID, note string) Plan {
	s.mu.Lock()
	plan, ok := s.plans[chamberID]
	if ok {
		plan.Status = StatusFailed
		plan.FailureNote = note
		s.plans[chamberID] = plan
	}
	s.mu.Unlock()
	return plan
}

func (s *Service) ensureFresh(plan Plan, cycleValue model.TransitCycle) error {
	if !cycleValue.IdentityMatches(plan.CycleID, plan.Generation) {
		return errors.New("current transit generation no longer matches the plan")
	}
	if cycleValue.Stage != model.StageLeveling || cycleValue.Stage.Terminal() {
		return fmt.Errorf("cycle is %s; leveling is not active", cycleValue.Stage)
	}
	cfg := s.Config(plan.ChamberID)
	snapshot := s.readings.SnapshotFor(plan.ChamberID, plan.CycleID, plan.Generation, cfg)
	if !snapshot.HasAll || !snapshot.AllFresh {
		return errors.New("refusing to command valves: upstream, downstream and chamber samples must all be fresh")
	}
	return nil
}

func (s *Service) activeCycle(chamberID string) (model.TransitCycle, error) {
	cycleValue, ok := s.cycles.Current(chamberID)
	if !ok || cycleValue.Stage.Terminal() {
		return model.TransitCycle{}, errors.New("no active transit for this chamber")
	}
	return cycleValue, nil
}

func (s *Service) levelingCycle(chamberID string) (model.TransitCycle, error) {
	cycleValue, err := s.activeCycle(chamberID)
	if err != nil {
		return model.TransitCycle{}, err
	}
	if cycleValue.Stage != model.StageLeveling {
		return model.TransitCycle{}, fmt.Errorf("cycle is %s; leveling targets are only produced while leveling", cycleValue.Stage)
	}
	return cycleValue, nil
}

// CurrentPlan returns the stored plan for a chamber regardless of status.
func (s *Service) CurrentPlan(chamberID string) (Plan, bool) {
	s.mu.Lock()
	plan, ok := s.plans[chamberID]
	s.mu.Unlock()
	return plan, ok
}

// View aggregates everything an operator display needs in one read.
type View struct {
	ChamberID string              `json:"chamber_id"`
	Cycle     *model.TransitCycle `json:"cycle,omitempty"`
	Config    ChamberConfig       `json:"config"`
	Readings  *Snapshot           `json:"readings,omitempty"`
	Plan      *Plan               `json:"plan,omitempty"`
	UpdatedAt time.Time           `json:"updated_at"`
}

func (s *Service) View(chamberID string) View {
	now := s.now().UTC()
	cfg := s.Config(chamberID)
	view := View{ChamberID: chamberID, Config: cfg, UpdatedAt: now}
	if cycleValue, ok := s.cycles.Current(chamberID); ok {
		cycleCopy := cycleValue
		view.Cycle = &cycleCopy
		snapshot := s.readings.SnapshotFor(chamberID, cycleCopy.ID, cycleCopy.Generation, cfg)
		view.Readings = &snapshot
	}
	if plan, ok := s.CurrentPlan(chamberID); ok {
		planCopy := plan
		view.Plan = &planCopy
	}
	return view
}
