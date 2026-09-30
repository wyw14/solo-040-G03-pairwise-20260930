package leveling

import (
	"encoding/json"
	"testing"
	"time"
)

func TestDefaultConfigValidates(t *testing.T) {
	if err := DefaultConfig().Validate(); err != nil {
		t.Fatalf("default config must be valid: %v", err)
	}
}

func TestConfigWithDefaults(t *testing.T) {
	filled := ChamberConfig{}.WithDefaults()
	defaults := DefaultConfig()
	if filled != defaults {
		t.Fatalf("zero config should resolve to defaults, got %+v", filled)
	}
}

func TestConfigValidateRejectsBadValues(t *testing.T) {
	cases := map[string]ChamberConfig{
		"tolerance above coarse delta": {UpstreamNormalM: 10, DownstreamNormalM: 4, PoolDriftM: 1, TargetToleranceM: 1, SampleFreshness: time.Second, CoarseOpening: .8, FineOpening: .25, CoarseDeltaM: .5, MaxSteps: 8},
		"fine above coarse":            {UpstreamNormalM: 10, DownstreamNormalM: 4, PoolDriftM: 1, TargetToleranceM: .05, SampleFreshness: time.Second, CoarseOpening: .2, FineOpening: .5, CoarseDeltaM: .5, MaxSteps: 8},
		"freshness too long":           {UpstreamNormalM: 10, DownstreamNormalM: 4, PoolDriftM: 1, TargetToleranceM: .05, SampleFreshness: time.Hour, CoarseOpening: .8, FineOpening: .25, CoarseDeltaM: .5, MaxSteps: 8},
		"max steps too small":          {UpstreamNormalM: 10, DownstreamNormalM: 4, PoolDriftM: 1, TargetToleranceM: .05, SampleFreshness: time.Second, CoarseOpening: .8, FineOpening: .25, CoarseDeltaM: .5, MaxSteps: 1},
	}
	for name, config := range cases {
		t.Run(name, func(t *testing.T) {
			if err := config.Validate(); err == nil {
				t.Fatalf("expected validation error")
			}
		})
	}
}

func TestConfigJSONRoundTrip(t *testing.T) {
	config := DefaultConfig()
	data, err := json.Marshal(config)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded ChamberConfig
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if decoded.SampleFreshness != config.SampleFreshness {
		t.Fatalf("freshness lost: got %v want %v", decoded.SampleFreshness, config.SampleFreshness)
	}
	if decoded != config {
		t.Fatalf("round trip mismatch: %+v vs %+v", decoded, config)
	}
}
