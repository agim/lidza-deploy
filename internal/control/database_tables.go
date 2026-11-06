package control

import (
	"net/http"
	"net/url"
	"regexp"

	"github.com/agim/lidza-deploy/internal/agent"
)

// Tables whose data a database's backups skip, chosen from the panel. The
// agent lists the database's tables with estimated rows and sizes and
// validates a saved list (the tables exist; at least one table keeps its
// data). Both routes require infrastructure.manage: the listing names the
// database's tables, and the setting changes what a restore contains.

var databaseResourceID = regexp.MustCompile(`^[a-z][a-z0-9-]{0,47}$`)

// databaseTables lists a database's user tables, largest first.
func (c *Control) databaseTables(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !databaseResourceID.MatchString(id) {
		http.NotFound(w, r)
		return
	}
	var tables agent.DatabaseTables
	if err := c.agentCall(r, r.PathValue("server"), http.MethodGet, "/v1/databases/"+url.PathEscape(id)+"/tables", nil, &tables); err != nil {
		failAgent(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	agent.JSON(w, 200, tables)
}

// databaseExcludedTables saves the tables whose data a database's backups
// skip; an empty list makes backups complete again.
func (c *Control) databaseExcludedTables(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !databaseResourceID.MatchString(id) {
		http.NotFound(w, r)
		return
	}
	var in agent.ExcludedTablesRequest
	if err := agent.Decode(w, r, &in); err != nil {
		agent.Fail(w, 400, err)
		return
	}
	if in.Tables == nil {
		in.Tables = []string{}
	}
	if err := c.agentCall(r, r.PathValue("server"), http.MethodPatch, "/v1/databases/"+url.PathEscape(id)+"/backups/excluded-tables", in, nil); err != nil {
		failAgent(w, err)
		return
	}
	agent.JSON(w, 200, map[string]string{"status": "saved"})
}
