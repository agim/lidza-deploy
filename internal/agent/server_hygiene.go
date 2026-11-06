package agent

import (
	"sync"
	"time"
)

// agentStarted is when this agent process started. Temporary files of a
// transfer that are older belong to an earlier process, which can no longer
// finish or remove them; tests move it.
var agentStarted = time.Now()

// hygieneState is runtime-only coordination of the agent's disk hygiene:
// stale transfer files, ClickHouse system log retention and Docker cleanup.
type hygieneState struct {
	mu          sync.Mutex
	logsChecked time.Time
	logsRunning bool
	logBytes    map[string]int64
}

var hygieneStates sync.Map // *Manager -> *hygieneState

func (m *Manager) hygiene() *hygieneState {
	value, _ := hygieneStates.LoadOrStore(m, &hygieneState{})
	return value.(*hygieneState)
}
