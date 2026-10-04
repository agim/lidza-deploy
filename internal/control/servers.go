package control

import (
	"errors"
	"github.com/agim/lidza-deploy/internal/agent"
	"net/http"
	"net/url"
	"regexp"
)

func validateServers(servers []Server) error {
	ids, urls := map[string]bool{}, map[string]bool{}
	if len(servers) > 100 {
		return errors.New("at most 100 servers")
	}
	for _, s := range servers {
		u, err := url.Parse(s.URL)
		if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path != "" || len(s.Token) < 32 || !regexp.MustCompile(`^[a-z][a-z0-9-]{0,47}$`).MatchString(s.ID) || len(s.Name) > 100 || ids[s.ID] || urls[s.URL] {
			return errors.New("invalid or duplicate agent configuration")
		}
		if u.Scheme != "https" && !(u.Scheme == "http" && (u.Hostname() == "127.0.0.1" || u.Hostname() == "localhost")) {
			return errors.New("remote agents require HTTPS")
		}
		ids[s.ID], urls[s.URL] = true, true
	}
	return nil
}
func (c *Control) servers() []Server {
	c.registryMu.RLock()
	defer c.registryMu.RUnlock()
	return append([]Server{}, c.registry...)
}

// Caller holds mu. Publish a new registry only after its encrypted state is durable.
func (c *Control) saveServers(next []Server) error {
	old := c.data.Servers
	c.data.Servers = next
	if err := c.save(); err != nil {
		c.data.Servers = old
		return err
	}
	c.registryMu.Lock()
	c.registry = append([]Server{}, next...)
	c.registryMu.Unlock()
	return nil
}
func (c *Control) addServer(w http.ResponseWriter, r *http.Request)  { c.writeServer(w, r, false) }
func (c *Control) editServer(w http.ResponseWriter, r *http.Request) { c.writeServer(w, r, true) }
func (c *Control) writeServer(w http.ResponseWriter, r *http.Request, edit bool) {
	var input Server
	if err := agent.Decode(w, r, &input); err != nil {
		agent.Fail(w, 400, err)
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	next := c.servers()
	index := -1
	if edit {
		input.ID = r.PathValue("id")
	}
	for i, s := range next {
		if s.ID == input.ID {
			index = i
		}
	}
	if edit && index < 0 {
		http.NotFound(w, r)
		return
	}
	if !edit && index >= 0 {
		agent.Fail(w, 409, errors.New("server ID already exists"))
		return
	}
	if edit && input.Token == "" {
		input.Token = next[index].Token
	}
	if edit {
		next[index] = input
	} else {
		next = append(next, input)
	}
	if err := validateServers(next); err != nil {
		agent.Fail(w, 400, err)
		return
	}
	var remote []agent.App
	if err := c.agentRequest(r, input, "GET", "/v1/apps", nil, &remote); err != nil {
		agent.Fail(w, 502, err)
		return
	}
	// Editing an endpoint must not silently move registered apps to an unrelated agent.
	for _, a := range c.data.Apps {
		if a.ServerID != input.ID {
			continue
		}
		found := false
		for _, b := range remote {
			if a.ID == b.ID && a.Repository == b.Repository && a.Domain == b.Domain {
				found = true
				break
			}
		}
		if !found {
			agent.Fail(w, 409, errors.New("agent is missing a registered application; server migration is not supported"))
			return
		}
	}
	if err := c.saveServers(next); err != nil {
		agent.Fail(w, 500, err)
		return
	}
	input.Token = ""
	code := 201
	if edit {
		code = 200
	}
	agent.JSON(w, code, input)
}
func (c *Control) removeServer(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	defer c.mu.Unlock()
	id := r.PathValue("id")
	for _, a := range c.data.Apps {
		if a.ServerID == id {
			agent.Fail(w, 409, errors.New("server still has registered applications"))
			return
		}
	}
	next := c.servers()
	for i, s := range next {
		if s.ID == id {
			next = append(next[:i], next[i+1:]...)
			if err := c.saveServers(next); err != nil {
				agent.Fail(w, 500, err)
				return
			}
			agent.JSON(w, 200, map[string]string{"status": "removed"})
			return
		}
	}
	http.NotFound(w, r)
}
