package hydraulics

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/wyw14/cry-96/internal/gate"
	"github.com/wyw14/cry-96/internal/journal"
	"github.com/wyw14/cry-96/internal/model"
)

// Controller coordinates valve commands, level windows and ramp operations.
type Controller struct {
	mu       sync.RWMutex
	commands *gate.Commands
	recorder *journal.Recorder
	windows  map[string]*LevelWindow
	ramp     *Ramp
	now      func() time.Time
}

func NewController(commands *gate.Commands, recorder *journal.Recorder, ramp *Ramp, now func() time.Time) *Controller {
	if now == nil {
		now = time.Now
	}
	return &Controller{
		commands: commands, recorder: recorder, windows: make(map[string]*LevelWindow),
		ramp: ramp, now: now,
	}
}

func (c *Controller) window(chamberID string) (*LevelWindow, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	window := c.windows[chamberID]
	if window != nil {
		return window, nil
	}
	created, err := NewLevelWindow(4)
	if err != nil {
		return nil, err
	}
	c.windows[chamberID] = created
	return created, nil
}

func (c *Controller) BeginLeveling(cycle model.TransitCycle) error {
	window, err := c.window(cycle.ChamberID)
	if err != nil {
		return err
	}
	window.Begin(cycle.ID, cycle.Generation)
	return nil
}

func (c *Controller) Observe(sample model.LevelSample) (bool, error) {
	window, err := c.windowForCycle(sample.CycleID, sample.Generation)
	if err != nil {
		return false, err
	}
	if err := window.Add(sample); err != nil {
		return false, err
	}
	return window.Stable(0.05), nil
}

func (c *Controller) windowForCycle(cycleID interface{ String() string }, generation uint64) (*LevelWindow, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	for _, window := range c.windows {
		id, currentGeneration := window.Identity()
		if id.String() == cycleID.String() && currentGeneration == generation {
			return window, nil
		}
	}
	return nil, errors.New("no active level window for transit")
}

func (c *Controller) issueDevice(ctx context.Context, cycle model.TransitCycle, device model.DeviceKind, opening float64) (model.DeviceCommand, error) {
	command, err := c.commands.Issue(cycle, device, "set_opening", opening, c.now())
	if err != nil {
		return model.DeviceCommand{}, err
	}
	_, err = c.recorder.Record(ctx, cycle.ChamberID, model.EventCommandQueued, cycle.ID, cycle.Generation, command)
	if err != nil {
		return model.DeviceCommand{}, err
	}
	return command, nil
}

// IssueValve keeps the historical signed-target surface: a positive opening
// drives the fill valve, a negative opening the drain valve.
func (c *Controller) IssueValve(ctx context.Context, cycle model.TransitCycle, target float64) error {
	_, err := c.IssueValveCommand(ctx, cycle, target)
	return err
}

// IssueValveCommand issues one signed valve opening and returns the command so
// callers (e.g. the leveling planner) can correlate it with their own steps.
func (c *Controller) IssueValveCommand(ctx context.Context, cycle model.TransitCycle, target float64) (model.DeviceCommand, error) {
	device := model.DeviceFillValve
	opening := target
	if target < 0 {
		device = model.DeviceDrainValve
		opening = -target
	}
	return c.issueDevice(ctx, cycle, device, opening)
}

// CloseValves commands both valves to a zero opening, used to leave the
// hydraulic system safe when a fenced leveling ramp aborts midway.
func (c *Controller) CloseValves(ctx context.Context, cycle model.TransitCycle) error {
	if _, err := c.issueDevice(ctx, cycle, model.DeviceFillValve, 0); err != nil {
		return err
	}
	_, err := c.issueDevice(ctx, cycle, model.DeviceDrainValve, 0)
	return err
}

// RampWith runs the existing ramp with a caller-supplied issuer so each step
// can be fenced against the live transit generation before a valve is driven.
func (c *Controller) RampWith(ctx context.Context, cycle model.TransitCycle, targets []float64, issue StepIssuer) (RampResult, error) {
	if issue == nil {
		issue = c.IssueValve
	}
	return c.ramp.Run(ctx, cycle, targets, issue)
}

func (c *Controller) RampTo(ctx context.Context, cycle model.TransitCycle, targets []float64) (RampResult, error) {
	return c.ramp.Run(ctx, cycle, targets, c.IssueValve)
}

func (c *Controller) WindowSamples(chamberID string) []model.LevelSample {
	c.mu.RLock()
	window := c.windows[chamberID]
	c.mu.RUnlock()
	if window == nil {
		return nil
	}
	return window.Samples()
}
