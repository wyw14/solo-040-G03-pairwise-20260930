package leveling

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/wyw14/cry-96/internal/hydraulics"
	"github.com/wyw14/cry-96/internal/journal"
	"github.com/wyw14/cry-96/internal/model"
)

// ErrPlanNotExecutable wraps the blocking reasons of a plan that has fresh
// data but cannot run (for example the transit is not in the leveling stage).
var ErrPlanNotExecutable = errors.New("leveling plan is not executable")

// CurrentCycleLookup returns the active transit generation for a chamber.
type CurrentCycleLookup interface {
	Current(chamberID string) (model.TransitCycle, bool)
}

// ObserveResult reports the outcome of ingesting one level reading.
type ObserveResult struct {
	Stored bool `json:"stored"`
	// Stable is only meaningful for chamber readings and mirrors the
	// hydraulics stability window.
	Stable bool `json:"stable"`
}

// Execution pairs the freshly rebuilt plan with the ramp that was issued.
type Execution struct {
	Plan Plan                  `json:"plan"`
	Ramp hydraulics.RampResult `json:"ramp"`
}

// Service owns per-chamber leveling configuration and the newest reading of
// each water surface. It never executes a plan handed in by a caller: the
// executable instructions are always rebuilt from the current transit
// generation and fresh samples at execution time.
type Service struct {
	mu         sync.RWMutex
	configs    map[string]ChamberConfig
	latest     map[string]map[model.Reach]model.ReachLevelSample
	cycles     CurrentCycleLookup
	hydraulics *hydraulics.Controller
	recorder   *journal.Recorder
	now        func() time.Time
}

func NewService(cycles CurrentCycleLookup, hydraulicsController *hydraulics.Controller, recorder *journal.Recorder, now func() time.Time) *Service {
	if now == nil {
		now = time.Now
	}
	return &Service{
		configs:    make(map[string]ChamberConfig),
		latest:     make(map[string]map[model.Reach]model.ReachLevelSample),
		cycles:     cycles,
		hydraulics: hydraulicsController,
		recorder:   recorder,
		now:        now,
	}
}

// Register seeds default configuration for a chamber.
func (s *Service) Register(chamberID string) {
	s.mu.Lock()
	if _, ok := s.configs[chamberID]; !ok {
		s.configs[chamberID] = DefaultConfig()
	}
	if s.latest[chamberID] == nil {
		s.latest[chamberID] = make(map[model.Reach]model.ReachLevelSample)
	}
	s.mu.Unlock()
}

// Configure replaces the validated leveling parameters of a chamber.
func (s *Service) Configure(chamberID string, config ChamberConfig) (ChamberConfig, error) {
	if chamberID == "" {
		return ChamberConfig{}, errors.New("chamber id is required")
	}
	config = config.WithDefaults()
	if err := config.Validate(); err != nil {
		return ChamberConfig{}, err
	}
	s.mu.Lock()
	s.configs[chamberID] = config
	s.mu.Unlock()
	return config, nil
}

// Config returns the configured parameters, applying defaults if unset.
func (s *Service) Config(chamberID string) ChamberConfig {
	s.mu.RLock()
	config, ok := s.configs[chamberID]
	s.mu.RUnlock()
	if !ok {
		return DefaultConfig()
	}
	return config
}

// Observe stores a reading for its reach. The reading must belong to the
// chamber's current transit generation; stale-generation telemetry is
// rejected so it can never feed a plan.
func (s *Service) Observe(ctx context.Context, chamberID string, sample model.ReachLevelSample) (ObserveResult, error) {
	cycleValue, ok := s.cycles.Current(chamberID)
	if !ok {
		return ObserveResult{}, errors.New("active transit not found")
	}
	if !cycleValue.IdentityMatches(sample.CycleID, sample.Generation) {
		return ObserveResult{}, errors.New("level sample belongs to an inactive transit generation")
	}

	result := ObserveResult{Stored: true}
	if sample.Reach == model.ReachChamber {
		stable, err := s.hydraulics.Observe(model.LevelSample{
			CycleID:    sample.CycleID,
			Generation: sample.Generation,
			Meters:     sample.Meters,
			ObservedAt: sample.ObservedAt,
		})
		if err != nil {
			return ObserveResult{}, err
		}
		result.Stable = stable
	}

	s.mu.Lock()
	if s.latest[chamberID] == nil {
		s.latest[chamberID] = make(map[model.Reach]model.ReachLevelSample)
	}
	s.latest[chamberID][sample.Reach] = sample
	s.mu.Unlock()

	if _, err := s.recorder.Record(ctx, chamberID, model.EventLevelObserved, sample.CycleID, sample.Generation, sample); err != nil {
		return ObserveResult{}, err
	}
	return result, nil
}

// LatestSamples returns the cached readings in a stable order.
func (s *Service) LatestSamples(chamberID string) []model.ReachLevelSample {
	s.mu.RLock()
	defer s.mu.RUnlock()
	readings := s.latest[chamberID]
	result := make([]model.ReachLevelSample, 0, 3)
	for _, reach := range []model.Reach{model.ReachUpstream, model.ReachChamber, model.ReachDownstream} {
		if sample, ok := readings[reach]; ok {
			result = append(result, sample)
		}
	}
	return result
}

// BuildPlan derives the current (possibly non-executable) plan. The plan
// carries the samples, ages and blocking reasons that explain the decision.
func (s *Service) BuildPlan(chamberID string) (Plan, error) {
	cycleValue, ok := s.cycles.Current(chamberID)
	if !ok {
		return Plan{}, errors.New("active transit not found")
	}
	s.mu.RLock()
	config := s.configs[chamberID]
	readings := s.latest[chamberID]
	latest := make(map[model.Reach]model.ReachLevelSample, len(readings))
	for reach, sample := range readings {
		latest[reach] = sample
	}
	s.mu.RUnlock()
	return Build(config.WithDefaults(), cycleValue, latest, s.now()), nil
}

// Execute rebuilds the plan from fresh samples and, only if it is
// executable, issues its valve ramp. A plan from an earlier transit or one
// computed before samples went stale can never reach the valves.
func (s *Service) Execute(ctx context.Context, chamberID string) (Execution, error) {
	cycleValue, ok := s.cycles.Current(chamberID)
	if !ok {
		return Execution{}, errors.New("active transit not found")
	}
	plan, err := s.BuildPlan(chamberID)
	if err != nil {
		return Execution{}, err
	}
	if plan.CycleID != cycleValue.ID || plan.Generation != cycleValue.Generation {
		return Execution{}, errors.New("leveling plan generation is no longer current")
	}
	if !plan.Executable {
		return Execution{Plan: plan}, fmt.Errorf("%w: %s", ErrPlanNotExecutable, strings.Join(plan.BlockedReasons, "; "))
	}

	ramp, err := s.hydraulics.RampTo(ctx, cycleValue, plan.Targets())
	if err != nil {
		return Execution{Plan: plan}, err
	}
	if _, err := s.recorder.Record(ctx, chamberID, model.EventPlanExecuted, cycleValue.ID, cycleValue.Generation, struct {
		Plan Plan                  `json:"plan"`
		Ramp hydraulics.RampResult `json:"ramp"`
	}{Plan: plan, Ramp: ramp}); err != nil {
		return Execution{}, err
	}
	return Execution{Plan: plan, Ramp: ramp}, nil
}
