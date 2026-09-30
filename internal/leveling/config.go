package leveling

import (
	"errors"
	"time"
)

// Role identifies which water surface a level reading describes.
type Role string

const (
	RoleUpstream   Role = "upstream"
	RoleDownstream Role = "downstream"
	RoleChamber    Role = "chamber"
)

func (r Role) Valid() bool {
	return r == RoleUpstream || r == RoleDownstream || r == RoleChamber
}

// ChamberConfig holds the leveling parameters that differ per chamber.
//
// ToleranceM is the band around the target pool level inside which the chamber
// is considered level. MaxStepM bounds the elevation change of a single valve
// step so the chamber is never commanded to jump the full head at once.
// MaxOpeningPercent bounds every issued valve opening (0..100). FinalOpening
// is the small "creep" opening used for the last step near target; the last
// step always closes (0) to stop the flow once the band is reached.
// Freshness is the maximum age a sample may have and still drive a plan.
type ChamberConfig struct {
	ToleranceM          float64       `json:"tolerance_m"`
	MaxStepM            float64       `json:"max_step_m"`
	MaxOpeningPercent   float64       `json:"max_opening_percent"`
	FinalOpeningPercent float64       `json:"final_opening_percent"`
	Freshness           time.Duration `json:"-"`
	FreshnessSeconds    float64       `json:"freshness_seconds"`
}

// DefaultConfig is used when a chamber has no explicit configuration.
func DefaultConfig() ChamberConfig {
	return ChamberConfig{
		ToleranceM:          0.10,
		MaxStepM:            1.0,
		MaxOpeningPercent:   40,
		FinalOpeningPercent: 15,
		Freshness:           30 * time.Second,
	}
}

func (c ChamberConfig) normalized() ChamberConfig {
	defaults := DefaultConfig()
	if c.ToleranceM <= 0 {
		c.ToleranceM = defaults.ToleranceM
	}
	if c.MaxStepM <= 0 {
		c.MaxStepM = defaults.MaxStepM
	}
	if c.MaxOpeningPercent <= 0 || c.MaxOpeningPercent > 100 {
		c.MaxOpeningPercent = defaults.MaxOpeningPercent
	}
	if c.FinalOpeningPercent <= 0 || c.FinalOpeningPercent > c.MaxOpeningPercent {
		c.FinalOpeningPercent = defaults.FinalOpeningPercent
		if c.FinalOpeningPercent > c.MaxOpeningPercent {
			c.FinalOpeningPercent = c.MaxOpeningPercent
		}
	}
	if c.Freshness <= 0 && c.FreshnessSeconds > 0 {
		c.Freshness = durationFromSeconds(c.FreshnessSeconds)
	}
	if c.Freshness <= 0 {
		c.Freshness = defaults.Freshness
	}
	c.FreshnessSeconds = c.Freshness.Seconds()
	return c
}

func (c ChamberConfig) validate() error {
	if c.ToleranceM <= 0 {
		return errors.New("tolerance_m must be positive")
	}
	if c.MaxStepM <= 0 {
		return errors.New("max_step_m must be positive")
	}
	if c.MaxOpeningPercent <= 0 || c.MaxOpeningPercent > 100 {
		return errors.New("max_opening_percent must be within (0,100]")
	}
	if c.FinalOpeningPercent < 0 || c.FinalOpeningPercent > c.MaxOpeningPercent {
		return errors.New("final_opening_percent must not exceed max_opening_percent")
	}
	if c.Freshness <= 0 {
		return errors.New("freshness must be positive")
	}
	return nil
}

func durationFromSeconds(value float64) time.Duration {
	return time.Duration(value * float64(time.Second))
}
