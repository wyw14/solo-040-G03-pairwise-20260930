package api

import (
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/wyw14/cry-96/internal/leveling"
)

type levelReadingInput struct {
	Role       string  `json:"role"`
	Meters     float64 `json:"meters"`
	ObservedAt string  `json:"observed_at,omitempty"`
}

type levelBatchInput struct {
	Readings []levelReadingInput `json:"readings"`
}

// levelObservationHandler ingests real-time upstream/downstream/chamber levels.
// The cycle generation is taken from the authoritative coordinator, never from
// the request body, so telemetry cannot be forged for another transit.
func levelObservationHandler(runtime *Runtime) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		chamberID := chi.URLParam(r, "chamberID")
		var input levelBatchInput
		if err := decodeJSON(r, &input); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		if len(input.Readings) == 0 || len(input.Readings) > 3 {
			writeError(w, http.StatusBadRequest, errors.New("provide 1 to 3 readings"))
			return
		}
		recorded := make([]leveling.Reading, 0, len(input.Readings))
		for _, item := range input.Readings {
			role := leveling.Role(item.Role)
			if !role.Valid() {
				writeError(w, http.StatusBadRequest, errors.New("reading role must be upstream, downstream or chamber"))
				return
			}
			observedAt := time.Time{}
			if item.ObservedAt != "" {
				parsed, err := time.Parse(time.RFC3339, item.ObservedAt)
				if err != nil {
					writeError(w, http.StatusBadRequest, errors.New("observed_at must be RFC3339"))
					return
				}
				observedAt = parsed
			}
			reading, err := runtime.Leveling.Observe(chamberID, role, item.Meters, observedAt)
			if err != nil {
				writeError(w, http.StatusConflict, err)
				return
			}
			recorded = append(recorded, reading)
		}
		writeJSON(w, http.StatusAccepted, map[string]any{"recorded": recorded})
	}
}

func levelingViewsHandler(runtime *Runtime) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		views := make([]leveling.View, 0, len(runtime.Chambers))
		for _, chamberID := range runtime.Chambers {
			views = append(views, runtime.Leveling.View(chamberID))
		}
		writeJSON(w, http.StatusOK, views)
	}
}

func levelingViewHandler(runtime *Runtime) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, runtime.Leveling.View(chi.URLParam(r, "chamberID")))
	}
}

func levelingPlanHandler(runtime *Runtime) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		plan, err := runtime.Leveling.Plan(r.Context(), chi.URLParam(r, "chamberID"))
		if err != nil {
			writeError(w, http.StatusConflict, err)
			return
		}
		writeJSON(w, http.StatusCreated, plan)
	}
}

func levelingExecuteHandler(runtime *Runtime) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		plan, err := runtime.Leveling.Execute(r.Context(), chi.URLParam(r, "chamberID"))
		if err != nil {
			writeError(w, http.StatusConflict, err)
			return
		}
		writeJSON(w, http.StatusOK, plan)
	}
}

type levelConfigInput struct {
	ToleranceM          float64 `json:"tolerance_m"`
	MaxStepM            float64 `json:"max_step_m"`
	MaxOpeningPercent   float64 `json:"max_opening_percent"`
	FinalOpeningPercent float64 `json:"final_opening_percent"`
	FreshnessSeconds    float64 `json:"freshness_seconds"`
}

func levelingConfigHandler(runtime *Runtime) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var input levelConfigInput
		if err := decodeJSON(r, &input); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		cfg := leveling.ChamberConfig{
			ToleranceM: input.ToleranceM, MaxStepM: input.MaxStepM,
			MaxOpeningPercent: input.MaxOpeningPercent, FinalOpeningPercent: input.FinalOpeningPercent,
			FreshnessSeconds: input.FreshnessSeconds,
		}
		applied, err := runtime.Leveling.Configure(chi.URLParam(r, "chamberID"), cfg)
		if err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		writeJSON(w, http.StatusOK, applied)
	}
}
