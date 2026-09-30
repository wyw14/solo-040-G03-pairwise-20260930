// Package leveling turns transit direction, fresh three-reach level samples and
// the valve ramp capability into explainable target levels and step-by-step
// valve instructions. Plans are always bound to a transit cycle and its
// generation so that a plan computed for an old transit can never be executed
// against the current one.
package leveling

import (
	"encoding/json"
	"errors"
	"time"
)

// ChamberConfig holds the leveling parameters for one chamber.
type ChamberConfig struct {
	// UpstreamNormalM / DownstreamNormalM are the configured pool elevations
	// used to sanity-check live target-reach telemetry.
	UpstreamNormalM   float64 `json:"upstream_normal_m"`
	DownstreamNormalM float64 `json:"downstream_normal_m"`
	// PoolDriftM is how far a live pool reading may depart from the normal
	// elevation before the plan is blocked as implausible.
	PoolDriftM float64 `json:"pool_drift_m"`
	// TargetToleranceM is the acceptable final gap between chamber and target.
	TargetToleranceM float64 `json:"target_tolerance_m"`
	// SampleFreshness bounds the age of every reach sample used in a plan.
	SampleFreshness time.Duration `json:"-"`
	// CoarseOpening / FineOpening are the valve openings used for bulk and
	// trim flow; CoarseDeltaM is the level gap one coarse step is sized for.
	CoarseOpening float64 `json:"coarse_opening"`
	FineOpening   float64 `json:"fine_opening"`
	CoarseDeltaM  float64 `json:"coarse_delta_m"`
	// MaxSteps bounds the issued ramp including the fine trim and close.
	MaxSteps int `json:"max_steps"`
}

// DefaultConfig returns the conservative chamber profile used unless an
// operator configures one explicitly.
func DefaultConfig() ChamberConfig {
	return ChamberConfig{
		UpstreamNormalM:   10,
		DownstreamNormalM: 4,
		PoolDriftM:        1,
		TargetToleranceM:  0.05,
		SampleFreshness:   30 * time.Second,
		CoarseOpening:     0.8,
		FineOpening:       0.25,
		CoarseDeltaM:      0.5,
		MaxSteps:          8,
	}
}

// WithDefaults fills every unset field from DefaultConfig.
func (c ChamberConfig) WithDefaults() ChamberConfig {
	defaults := DefaultConfig()
	if c.UpstreamNormalM == 0 {
		c.UpstreamNormalM = defaults.UpstreamNormalM
	}
	if c.DownstreamNormalM == 0 {
		c.DownstreamNormalM = defaults.DownstreamNormalM
	}
	if c.PoolDriftM == 0 {
		c.PoolDriftM = defaults.PoolDriftM
	}
	if c.TargetToleranceM == 0 {
		c.TargetToleranceM = defaults.TargetToleranceM
	}
	if c.SampleFreshness == 0 {
		c.SampleFreshness = defaults.SampleFreshness
	}
	if c.CoarseOpening == 0 {
		c.CoarseOpening = defaults.CoarseOpening
	}
	if c.FineOpening == 0 {
		c.FineOpening = defaults.FineOpening
	}
	if c.CoarseDeltaM == 0 {
		c.CoarseDeltaM = defaults.CoarseDeltaM
	}
	if c.MaxSteps == 0 {
		c.MaxSteps = defaults.MaxSteps
	}
	return c
}

// Validate checks the physical and operational bounds of the parameters.
func (c ChamberConfig) Validate() error {
	if c.UpstreamNormalM <= 0 || c.DownstreamNormalM <= 0 {
		return errors.New("normal pool levels must be positive")
	}
	if c.PoolDriftM <= 0 {
		return errors.New("pool drift tolerance must be positive")
	}
	if c.TargetToleranceM <= 0 {
		return errors.New("target tolerance must be positive")
	}
	if c.TargetToleranceM >= c.CoarseDeltaM {
		return errors.New("target tolerance must be smaller than the coarse step delta")
	}
	if c.SampleFreshness <= 0 || c.SampleFreshness > 10*time.Minute {
		return errors.New("sample freshness must be between 1s and 10m")
	}
	if c.CoarseOpening <= 0 || c.CoarseOpening > 1 || c.FineOpening <= 0 || c.FineOpening > 1 {
		return errors.New("valve openings must be within (0,1]")
	}
	if c.FineOpening > c.CoarseOpening {
		return errors.New("fine opening must not exceed coarse opening")
	}
	if c.CoarseDeltaM <= 0 {
		return errors.New("coarse step delta must be positive")
	}
	if c.MaxSteps < 2 || c.MaxSteps > 12 {
		return errors.New("max steps must be between 2 and 12")
	}
	return nil
}

type chamberConfigJSON struct {
	UpstreamNormalM    float64 `json:"upstream_normal_m"`
	DownstreamNormalM  float64 `json:"downstream_normal_m"`
	PoolDriftM         float64 `json:"pool_drift_m"`
	TargetToleranceM   float64 `json:"target_tolerance_m"`
	SampleMaxAgeSecond float64 `json:"sample_max_age_seconds"`
	CoarseOpening      float64 `json:"coarse_opening"`
	FineOpening        float64 `json:"fine_opening"`
	CoarseDeltaM       float64 `json:"coarse_delta_m"`
	MaxSteps           int     `json:"max_steps"`
}

func (c ChamberConfig) MarshalJSON() ([]byte, error) {
	return json.Marshal(chamberConfigJSON{
		UpstreamNormalM:    c.UpstreamNormalM,
		DownstreamNormalM:  c.DownstreamNormalM,
		PoolDriftM:         c.PoolDriftM,
		TargetToleranceM:   c.TargetToleranceM,
		SampleMaxAgeSecond: c.SampleFreshness.Seconds(),
		CoarseOpening:      c.CoarseOpening,
		FineOpening:        c.FineOpening,
		CoarseDeltaM:       c.CoarseDeltaM,
		MaxSteps:           c.MaxSteps,
	})
}

func (c *ChamberConfig) UnmarshalJSON(data []byte) error {
	var decoded chamberConfigJSON
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	*c = ChamberConfig{
		UpstreamNormalM:   decoded.UpstreamNormalM,
		DownstreamNormalM: decoded.DownstreamNormalM,
		PoolDriftM:        decoded.PoolDriftM,
		TargetToleranceM:  decoded.TargetToleranceM,
		SampleFreshness:   time.Duration(decoded.SampleMaxAgeSecond * float64(time.Second)),
		CoarseOpening:     decoded.CoarseOpening,
		FineOpening:       decoded.FineOpening,
		CoarseDeltaM:      decoded.CoarseDeltaM,
		MaxSteps:          decoded.MaxSteps,
	}
	return nil
}
