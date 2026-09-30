package api

import (
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/wyw14/cry-96/internal/leveling"
	"github.com/wyw14/cry-96/internal/model"
)

type levelSampleInput struct {
	Reach      model.Reach `json:"reach"`
	Meters     float64     `json:"meters"`
	ObservedAt *time.Time  `json:"observed_at,omitempty"`
}

type levelSampleBatch struct {
	Samples []levelSampleInput `json:"samples"`
}

type levelSampleAck struct {
	Reach      model.Reach `json:"reach"`
	Meters     float64     `json:"meters"`
	Stable     bool        `json:"stable"`
	ObservedAt time.Time   `json:"observed_at"`
}

// levelSamplesHandler ingests telemetry for the active transit. The cycle
// identity is stamped server-side from the current generation, so clients
// can never attach readings to an old transit.
func levelSamplesHandler(runtime *Runtime) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		chamberID := chi.URLParam(r, "chamberID")
		cycleValue, ok := runtime.Cycles.Current(chamberID)
		if !ok {
			writeError(w, http.StatusConflict, errors.New("active transit not found"))
			return
		}
		var batch levelSampleBatch
		if err := decodeJSON(r, &batch); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		if len(batch.Samples) == 0 || len(batch.Samples) > 3 {
			writeError(w, http.StatusBadRequest, errors.New("samples must contain 1 to 3 reach readings"))
			return
		}
		seen := map[model.Reach]bool{}
		acks := make([]levelSampleAck, 0, len(batch.Samples))
		for _, input := range batch.Samples {
			if !input.Reach.Valid() {
				writeError(w, http.StatusBadRequest, errors.New("invalid level reach"))
				return
			}
			if seen[input.Reach] {
				writeError(w, http.StatusBadRequest, errors.New("duplicate reading for reach "+string(input.Reach)))
				return
			}
			seen[input.Reach] = true
			observedAt := runtime.Now().UTC()
			if input.ObservedAt != nil {
				observedAt = input.ObservedAt.UTC()
			}
			sample, err := model.NewReachLevelSample(cycleValue.ID, cycleValue.Generation, input.Reach, input.Meters, observedAt)
			if err != nil {
				writeError(w, http.StatusBadRequest, err)
				return
			}
			result, err := runtime.Leveling.Observe(r.Context(), chamberID, sample)
			if err != nil {
				writeError(w, http.StatusConflict, err)
				return
			}
			acks = append(acks, levelSampleAck{
				Reach: sample.Reach, Meters: sample.Meters,
				Stable: result.Stable, ObservedAt: sample.ObservedAt,
			})
		}
		writeJSON(w, http.StatusAccepted, map[string]any{
			"cycle_id": cycleValue.ID, "generation": cycleValue.Generation, "samples": acks,
		})
	}
}

func levelPlanHandler(runtime *Runtime) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		plan, err := runtime.Leveling.BuildPlan(chi.URLParam(r, "chamberID"))
		if err != nil {
			writeError(w, http.StatusConflict, err)
			return
		}
		writeJSON(w, http.StatusOK, plan)
	}
}

func levelExecuteHandler(runtime *Runtime) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		execution, err := runtime.Leveling.Execute(r.Context(), chi.URLParam(r, "chamberID"))
		if err != nil {
			// A non-executable plan carries a full diagnostic body; surface
			// the rebuilt plan alongside the error so operators see why.
			if errors.Is(err, leveling.ErrPlanNotExecutable) {
				writeJSON(w, http.StatusConflict, map[string]any{
					"error": err.Error(), "plan": execution.Plan,
				})
				return
			}
			writeError(w, http.StatusConflict, err)
			return
		}
		writeJSON(w, http.StatusAccepted, execution)
	}
}

func levelConfigHandler(runtime *Runtime) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, runtime.Leveling.Config(chi.URLParam(r, "chamberID")))
	}
}

func updateLevelConfigHandler(runtime *Runtime) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var config leveling.ChamberConfig
		if err := decodeJSON(r, &config); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		saved, err := runtime.Leveling.Configure(chi.URLParam(r, "chamberID"), config)
		if err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		writeJSON(w, http.StatusOK, saved)
	}
}
