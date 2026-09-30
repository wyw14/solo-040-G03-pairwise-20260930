package leveling

import (
	"errors"
	"sync"
	"time"

	"github.com/google/uuid"
)

// Reading is one real-time water level observation tied to a transit generation.
type Reading struct {
	ChamberID  string    `json:"chamber_id"`
	Role       Role      `json:"role"`
	CycleID    uuid.UUID `json:"cycle_id"`
	Generation uint64    `json:"generation"`
	Meters     float64   `json:"meters"`
	ObservedAt time.Time `json:"observed_at"`
}

func (r Reading) Fresh(cfg ChamberConfig, now time.Time) bool {
	age := now.Sub(r.ObservedAt)
	return age >= 0 && age <= cfg.Freshness
}

// ReadingStore keeps the latest reading of each role for every chamber and
// rejects telemetry that does not belong to the current transit generation,
// so a stale cycle can never feed a plan.
type ReadingStore struct {
	mu     sync.RWMutex
	latest map[string]map[Role]Reading
	now    func() time.Time
}

func NewReadingStore(now func() time.Time) *ReadingStore {
	if now == nil {
		now = time.Now
	}
	return &ReadingStore{latest: make(map[string]map[Role]Reading), now: now}
}

func (s *ReadingStore) Observe(cycleID uuid.UUID, generation uint64, chamberID string, role Role, meters float64, observedAt time.Time) (Reading, error) {
	if chamberID == "" {
		return Reading{}, errors.New("chamber id is required")
	}
	if !role.Valid() {
		return Reading{}, errors.New("reading role must be upstream, downstream or chamber")
	}
	if cycleID == uuid.Nil || generation == 0 {
		return Reading{}, errors.New("reading requires a transit cycle generation")
	}
	if meters < 0 {
		return Reading{}, errors.New("level in meters must not be negative")
	}
	if observedAt.IsZero() {
		observedAt = s.now()
	}
	reading := Reading{
		ChamberID: chamberID, Role: role, CycleID: cycleID,
		Generation: generation, Meters: meters, ObservedAt: observedAt.UTC(),
	}
	s.mu.Lock()
	roles := s.latest[chamberID]
	if roles == nil {
		roles = make(map[Role]Reading)
		s.latest[chamberID] = roles
	}
	roles[role] = reading
	s.mu.Unlock()
	return reading, nil
}

// Snapshot returns the three role readings for a chamber together with a
// freshness verdict against the supplied (already normalized) configuration.
type Snapshot struct {
	ChamberID string           `json:"chamber_id"`
	Readings  map[Role]Reading `json:"readings"`
	Fresh     map[Role]bool    `json:"fresh"`
	AllFresh  bool             `json:"all_fresh"`
	HasAll    bool             `json:"has_all"`
	Now       time.Time        `json:"now"`
}

func (s *ReadingStore) SnapshotFor(chamberID string, cycleID uuid.UUID, generation uint64, cfg ChamberConfig) Snapshot {
	now := s.now().UTC()
	s.mu.RLock()
	roles := s.latest[chamberID]
	s.mu.RUnlock()
	result := Snapshot{
		ChamberID: chamberID,
		Readings:  make(map[Role]Reading, 3),
		Fresh:     make(map[Role]bool, 3),
		AllFresh:  true,
		HasAll:    true,
		Now:       now,
	}
	for _, role := range []Role{RoleUpstream, RoleDownstream, RoleChamber} {
		reading, ok := roles[role]
		current := ok && reading.CycleID == cycleID && reading.Generation == generation
		fresh := current && reading.Fresh(cfg, now)
		result.Readings[role] = reading
		result.Fresh[role] = fresh
		if !current {
			result.HasAll = false
		}
		if !fresh {
			result.AllFresh = false
		}
	}
	return result
}
