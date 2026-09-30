package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/wyw14/cry-96/internal/leveling"
	"github.com/wyw14/cry-96/internal/model"
)

func newTestRouter(t *testing.T) (http.Handler, *Runtime) {
	t.Helper()
	runtime, err := NewRuntime(context.Background(), "")
	if err != nil {
		t.Fatalf("runtime: %v", err)
	}
	return Router(runtime, "."), runtime
}

func levelingCycleValue(chamberID string, direction model.Direction, generation uint64) model.TransitCycle {
	cycleValue, _ := model.NewTransitCycle(chamberID, uuid.New(), direction, generation, time.Now().UTC())
	for _, stage := range []model.TransitStage{model.StageAdmitting, model.StageSealed, model.StageLeveling} {
		_ = cycleValue.Advance(stage, "test", time.Now().UTC())
	}
	return cycleValue
}

func doJSON(t *testing.T, router http.Handler, method, path string, body any) (int, map[string]any) {
	t.Helper()
	var reader *bytes.Reader
	if body != nil {
		data, _ := json.Marshal(body)
		reader = bytes.NewReader(data)
	} else {
		reader = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, reader)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	var decoded map[string]any
	if rec.Body.Len() > 0 {
		_ = json.Unmarshal(rec.Body.Bytes(), &decoded)
	}
	return rec.Code, decoded
}

func doPlan(t *testing.T, router http.Handler, method, path string, body any) (int, leveling.Plan) {
	t.Helper()
	data, _ := json.Marshal(body)
	req := httptest.NewRequest(method, path, bytes.NewReader(data))
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	var plan leveling.Plan
	_ = json.Unmarshal(rec.Body.Bytes(), &plan)
	return rec.Code, plan
}

func TestLevelingEndToEnd(t *testing.T) {
	router, runtime := newTestRouter(t)
	chamber := "chamber-1"
	cycleValue := levelingCycleValue(chamber, model.DirectionUpstream, 1)
	runtime.Cycles.Restore(cycleValue)

	// Readings are rejected when a role is invalid.
	if code, _ := doJSON(t, router, http.MethodPost, "/api/chambers/"+chamber+"/leveling/readings",
		map[string]any{"readings": []map[string]any{{"role": "river", "meters": 1}}}); code != http.StatusBadRequest {
		t.Fatalf("invalid role status = %d, want 400", code)
	}

	readings := map[string]any{"readings": []map[string]any{
		{"role": "upstream", "meters": 10},
		{"role": "downstream", "meters": 2},
		{"role": "chamber", "meters": 5},
	}}
	if code, _ := doJSON(t, router, http.MethodPost, "/api/chambers/"+chamber+"/leveling/readings", readings); code != http.StatusAccepted {
		t.Fatalf("readings status = %d, want 202", code)
	}

	code, plan := doPlan(t, router, http.MethodPost, "/api/chambers/"+chamber+"/leveling/plan", nil)
	if code != http.StatusCreated {
		t.Fatalf("plan status = %d, want 201", code)
	}
	if plan.TargetRole != leveling.RoleUpstream || plan.TargetLevel != 10 || plan.Mode != leveling.ModeFill {
		t.Fatalf("unexpected plan: role=%s target=%.2f mode=%s", plan.TargetRole, plan.TargetLevel, plan.Mode)
	}
	if len(plan.Steps) == 0 {
		t.Fatal("fill plan must contain valve steps")
	}

	code, executed := doPlan(t, router, http.MethodPost, "/api/chambers/"+chamber+"/leveling/execute", nil)
	if code != http.StatusOK || executed.Status != leveling.StatusExecuted {
		t.Fatalf("execute status=%d plan=%+v", code, executed)
	}
	for i, step := range executed.Steps {
		if step.CommandID == nil {
			t.Fatalf("step %d missing command correlation", i)
		}
	}

	// An executed plan cannot run a second time.
	if code, _ := doJSON(t, router, http.MethodPost, "/api/chambers/"+chamber+"/leveling/execute", nil); code != http.StatusConflict {
		t.Fatalf("re-execute status = %d, want 409", code)
	}
}

func TestLevelingBlocksOldGeneration(t *testing.T) {
	router, runtime := newTestRouter(t)
	chamber := "chamber-1"
	first := levelingCycleValue(chamber, model.DirectionUpstream, 1)
	runtime.Cycles.Restore(first)

	doJSON(t, router, http.MethodPost, "/api/chambers/"+chamber+"/leveling/readings", map[string]any{"readings": []map[string]any{
		{"role": "upstream", "meters": 10}, {"role": "downstream", "meters": 2}, {"role": "chamber", "meters": 5},
	}})
	code, oldPlan := doPlan(t, router, http.MethodPost, "/api/chambers/"+chamber+"/leveling/plan", nil)
	if code != http.StatusCreated {
		t.Fatalf("plan status = %d", code)
	}

	// A new transit generation turns over before the old plan is executed.
	next := levelingCycleValue(chamber, model.DirectionUpstream, 2)
	runtime.Cycles.Restore(next)

	if code, _ := doJSON(t, router, http.MethodPost, "/api/chambers/"+chamber+"/leveling/plan", nil); code != http.StatusConflict {
		t.Fatalf("planning on foreign readings status = %d, want 409", code)
	}
	if code, _ := doJSON(t, router, http.MethodPost, "/api/chambers/"+chamber+"/leveling/execute", nil); code != http.StatusConflict {
		t.Fatalf("stale execute status = %d, want 409", code)
	}
	stored, ok := runtime.Leveling.CurrentPlan(chamber)
	if !ok || stored.ID != oldPlan.ID || stored.Status != leveling.StatusFailed {
		t.Fatalf("old plan must be retained and marked failed: %+v", stored)
	}
}

func TestLevelingRequiresActiveCycle(t *testing.T) {
	router, _ := newTestRouter(t)
	// chamber-2 has no transit at all.
	if code, _ := doJSON(t, router, http.MethodPost, "/api/chambers/chamber-2/leveling/readings",
		map[string]any{"readings": []map[string]any{{"role": "chamber", "meters": 3}}}); code != http.StatusConflict {
		t.Fatalf("reading without a cycle status = %d, want 409", code)
	}
}
