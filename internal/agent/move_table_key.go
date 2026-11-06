package agent

// Incremental keys for tables loaded before their key was recorded. A
// bulk copy made by an agent that did not yet record incremental keys, or
// a move whose checkpoint lost them, leaves the destination with a table
// that was loaded and verified but has no watermark. The final sync then
// derives one: the source names the table's key, the destination reports
// the highest key it holds and its row count, and the source confirms it
// holds exactly as many rows up to that key. The destination keeps the
// table and receives only the rows after it, as if the key had been
// recorded at the bulk copy.

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// MoveTableKeyRequest asks about one table: without Key, the table's
// incremental key as this agent detects it; with Key, the table's totals
// under that key and, when the key carries a watermark, the number of rows
// up to it.
type MoveTableKeyRequest struct {
	Table string        `json:"table"`
	Key   *MoveTableKey `json:"key,omitempty"`
}

// MoveTableKeyView answers a MoveTableKeyRequest.
type MoveTableKeyView struct {
	Table string `json:"table"`
	// Key is the detected key (with the table's current watermark and
	// statistics); nil when the table has none.
	Key *MoveTableKey `json:"key,omitempty"`
	// Total and Max are count(*) and max(key) when a key was given.
	Total int64  `json:"total"`
	Max   string `json:"max,omitempty"`
	// UpTo is the number of rows at or before the given watermark.
	UpTo int64 `json:"up_to"`
}

func (m *Manager) moveTableKeyRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/move/databases/{id}/table-key", m.moveTableKeyRoute)
}

func (m *Manager) moveTableKeyRoute(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var in MoveTableKeyRequest
	if err := Decode(w, r, &in); err != nil || !moveTableName.MatchString(in.Table) || !idPattern.MatchString(id) || (in.Key != nil && validMoveKey(*in.Key) != nil) {
		Fail(w, 400, errors.New("name one table as an exact schema.table name, with a valid key when given"))
		return
	}
	m.mu.Lock()
	_, err := m.moveDatabaseTargetLocked(id)
	m.mu.Unlock()
	if err != nil {
		Fail(w, 404, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Minute)
	defer cancel()
	out := MoveTableKeyView{Table: in.Table}
	if in.Key == nil {
		key, err := m.moveTableKeyOf(ctx, id, in.Table)
		if err != nil {
			Fail(w, 409, errors.New("the table's key could not be detected"))
			return
		}
		out.Key = key
		JSON(w, 200, out)
		return
	}
	out.Total, out.Max, err = m.moveTableTotals(ctx, id, in.Table, *in.Key)
	if err != nil {
		Fail(w, 409, err)
		return
	}
	if in.Key.Watermark != "" {
		text, err := movePSQL(ctx, m, id, moveSessionPins+"SELECT count(*) FROM "+moveTableIdent(in.Table)+" WHERE "+moveColumnIdent(in.Key.Column)+" <= "+moveKeyLiteral(*in.Key))
		if err != nil {
			Fail(w, 409, errors.New("the rows up to the watermark could not be counted"))
			return
		}
		n, err := strconv.ParseInt(strings.TrimSpace(text), 10, 64)
		if err != nil {
			Fail(w, 409, errors.New("the database returned an unexpected row count"))
			return
		}
		out.UpTo = n
	}
	JSON(w, 200, out)
}
