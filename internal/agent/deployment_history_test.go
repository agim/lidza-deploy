package agent

import (
	"encoding/json"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

func TestDeploymentHistorySummariesAndIndividualLogs(t *testing.T) {
	m := testManager(t, &fakeRuntime{})
	log := strings.Repeat("x", 65536)
	m.mu.Lock()
	for i := 0; i < 100; i++ {
		m.data.Deployments = append(m.data.Deployments, Deployment{ID: strconv.Itoa(i), AppID: "app", Status: "failed", Log: log, Error: "original failure"})
	}
	m.mu.Unlock()
	handler := Handler(m)
	call := func(path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", path, nil)
		req.Header.Set("Authorization", "Bearer "+m.cfg.APIKey)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		return w
	}
	legacy := call("/v1/deployments")
	if legacy.Code != 200 || legacy.Body.Len() <= 4<<20 {
		t.Fatal("fixture must reproduce oversized legacy history", legacy.Body.Len())
	}
	summary := call("/v1/deployments?logs=0")
	if summary.Code != 200 || summary.Body.Len() > 100000 {
		t.Fatal("history summary is not bounded", summary.Body.Len())
	}
	var rows []Deployment
	if err := json.Unmarshal(summary.Body.Bytes(), &rows); err != nil {
		t.Fatal(err)
	}
	if len(rows) != 100 || rows[0].Log != "" || rows[0].Error != "original failure" {
		t.Fatal("summary lost deployment metadata")
	}
	detail := call("/v1/deployments/99")
	var row Deployment
	if err := json.Unmarshal(detail.Body.Bytes(), &row); err != nil {
		t.Fatal(err)
	}
	if row.ID != "99" || row.Log != log || row.Error != "original failure" {
		t.Fatal("individual log was lost")
	}
	if call("/v1/deployments/unknown").Code != 404 {
		t.Fatal("unknown deployment detail accepted")
	}
	// Summary construction must never mutate the persisted/diagnostic log.
	if m.Deployments()[99].Log != log {
		t.Fatal("summary erased original logs")
	}
}

func TestDeploymentSummariesBudgetEscapedErrorsAcrossRetainedHistory(t *testing.T) {
	rows := make([]Deployment, 500)
	for i := range rows {
		rows[i] = deploymentSummary(Deployment{ID: strconv.Itoa(i), AppID: "fixture", Error: strings.Repeat("\x00", 10000), CommitMessage: strings.Repeat("\x00", 10000), Log: strings.Repeat("x", 65536), Changes: []string{strings.Repeat("x", 65536)}})
	}
	data, err := json.Marshal(rows)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) >= 4<<20 {
		t.Fatalf("escaped summary errors still exceed control limit: %d", len(data))
	}
}
