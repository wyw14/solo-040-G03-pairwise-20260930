package model

import (
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

type DeviceKind string

const (
	DeviceUpstreamGate   DeviceKind = "upstream_gate"
	DeviceDownstreamGate DeviceKind = "downstream_gate"
	DeviceFillValve      DeviceKind = "fill_valve"
	DeviceDrainValve     DeviceKind = "drain_valve"
)

type CommandState string

const (
	CommandQueued    CommandState = "queued"
	CommandAccepted  CommandState = "accepted"
	CommandExecuting CommandState = "executing"
	CommandSettled   CommandState = "settled"
	CommandCancelled CommandState = "cancelled"
	CommandFailed    CommandState = "failed"
)

func (s CommandState) Terminal() bool {
	return s == CommandSettled || s == CommandCancelled || s == CommandFailed
}

type DeviceCommand struct {
	ID         uuid.UUID    `json:"id"`
	CycleID    uuid.UUID    `json:"cycle_id"`
	ChamberID  string       `json:"chamber_id"`
	Generation uint64       `json:"generation"`
	Epoch      uint64       `json:"epoch"`
	Device     DeviceKind   `json:"device"`
	Action     string       `json:"action"`
	Target     float64      `json:"target,omitempty"`
	State      CommandState `json:"state"`
	CreatedAt  time.Time    `json:"created_at"`
	UpdatedAt  time.Time    `json:"updated_at"`
}

func NewDeviceCommand(cycle TransitCycle, epoch uint64, device DeviceKind, action string, target float64, now time.Time) (DeviceCommand, error) {
	if cycle.ID == uuid.Nil || cycle.ChamberID == "" {
		return DeviceCommand{}, errors.New("command requires a transit cycle")
	}
	if epoch == 0 {
		return DeviceCommand{}, errors.New("command epoch must be positive")
	}
	if device == "" || action == "" {
		return DeviceCommand{}, errors.New("device and action are required")
	}
	return DeviceCommand{
		ID: uuid.New(), CycleID: cycle.ID, ChamberID: cycle.ChamberID,
		Generation: cycle.Generation, Epoch: epoch, Device: device,
		Action: action, Target: target, State: CommandQueued,
		CreatedAt: now.UTC(), UpdatedAt: now.UTC(),
	}, nil
}

func (c *DeviceCommand) SetState(next CommandState, now time.Time) error {
	if c.State.Terminal() {
		return fmt.Errorf("command %s is already terminal", c.ID)
	}
	c.State = next
	c.UpdatedAt = now.UTC()
	return nil
}

type DeviceAck struct {
	CommandID  uuid.UUID  `json:"command_id"`
	CycleID    uuid.UUID  `json:"cycle_id"`
	ChamberID  string     `json:"chamber_id"`
	Generation uint64     `json:"generation"`
	Epoch      uint64     `json:"epoch"`
	Device     DeviceKind `json:"device"`
	Settled    bool       `json:"settled"`
	ObservedAt time.Time  `json:"observed_at"`
}

func (a DeviceAck) Matches(command DeviceCommand) bool {
	return a.CommandID == command.ID && a.CycleID == command.CycleID &&
		a.Generation == command.Generation && a.Epoch == command.Epoch &&
		a.ChamberID == command.ChamberID && a.Device == command.Device
}

type GatePosition string

const (
	GateOpen    GatePosition = "open"
	GateOpening GatePosition = "opening"
	GateClosed  GatePosition = "closed"
	GateClosing GatePosition = "closing"
)

func (p GatePosition) Sealed() bool { return p == GateClosed }

type LevelSample struct {
	CycleID    uuid.UUID `json:"cycle_id"`
	Generation uint64    `json:"generation"`
	Meters     float64   `json:"meters"`
	ObservedAt time.Time `json:"observed_at"`
}

// Reach identifies one of the three water surfaces relevant to leveling.
type Reach string

const (
	ReachUpstream   Reach = "upstream"
	ReachChamber    Reach = "chamber"
	ReachDownstream Reach = "downstream"
)

func (r Reach) Valid() bool {
	return r == ReachUpstream || r == ReachChamber || r == ReachDownstream
}

func (r Reach) Direction() (Direction, bool) {
	switch r {
	case ReachUpstream:
		return DirectionUpstream, true
	case ReachDownstream:
		return DirectionDownstream, true
	default:
		return "", false
	}
}

// ReachLevelSample is one telemetry reading for a single water surface.
type ReachLevelSample struct {
	CycleID    uuid.UUID `json:"cycle_id"`
	Generation uint64    `json:"generation"`
	Reach      Reach     `json:"reach"`
	Meters     float64   `json:"meters"`
	ObservedAt time.Time `json:"observed_at"`
}

// NewReachLevelSample validates a reading and normalizes its timestamp to UTC.
func NewReachLevelSample(cycleID uuid.UUID, generation uint64, reach Reach, meters float64, observedAt time.Time) (ReachLevelSample, error) {
	if cycleID == uuid.Nil || generation == 0 {
		return ReachLevelSample{}, errors.New("sample requires a transit cycle and generation")
	}
	if !reach.Valid() {
		return ReachLevelSample{}, fmt.Errorf("invalid level reach %q", reach)
	}
	if meters <= 0 {
		return ReachLevelSample{}, errors.New("level must be positive")
	}
	if observedAt.IsZero() {
		return ReachLevelSample{}, errors.New("sample observation time is required")
	}
	return ReachLevelSample{
		CycleID: cycleID, Generation: generation, Reach: reach,
		Meters: meters, ObservedAt: observedAt.UTC(),
	}, nil
}
