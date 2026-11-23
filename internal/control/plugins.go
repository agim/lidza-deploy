package control

// Plugins of a server (agent: plugins.go, docs/plugins-proposal.md). The
// control panel only relays: the server's agent verifies the signature,
// installs the plugin and runs its hooks.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/agim/lidza-deploy/internal/agent"
	"github.com/agim/lidza-deploy/internal/plugin"
)

var (
	pluginRouteName  = regexp.MustCompile(`^[a-z][a-z0-9-]{0,39}$`)
	pluginRepository = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,38}/[A-Za-z0-9_.-]{1,100}$`)
	pluginVersion    = regexp.MustCompile(`^[0-9]{1,5}(\.[0-9]{1,5}){0,2}$`)
)

// pluginReleaseFetch downloads one file of a plugin release into w, at most
// limit bytes; tests replace it. Redirects are followed only to GitHub's own
// download hosts.
var pluginReleaseFetch = func(ctx context.Context, url string, limit int64, w io.Writer) (int64, error) {
	client := &http.Client{Timeout: 20 * time.Minute, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		host := req.URL.Hostname()
		if len(via) > 5 || req.URL.Scheme != "https" || !(host == "github.com" || strings.HasSuffix(host, ".githubusercontent.com")) {
			return errors.New("unexpected redirect")
		}
		return nil
	}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, err
	}
	res, err := client.Do(req)
	if err != nil {
		return 0, errors.New("the release could not be downloaded")
	}
	defer res.Body.Close()
	if res.StatusCode == http.StatusNotFound {
		return 0, errors.New("not found")
	}
	if res.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("the download answered %d", res.StatusCode)
	}
	n, err := io.Copy(w, io.LimitReader(res.Body, limit+1))
	if err != nil {
		return n, errors.New("the download was interrupted")
	}
	if n > limit {
		return n, errors.New("the file is larger than a plugin may be")
	}
	return n, nil
}

// installPluginRelease installs a plugin from its published release: the
// three files of release v<version> of a public GitHub repository. The
// control panel only fetches and relays them; the server's agent verifies
// the signature with the keys that server trusts and checks the archive
// against the signed manifest.
func (c *Control) installPluginRelease(r *http.Request, target Server, repository, version string) (json.RawMessage, error) {
	if !pluginRepository.MatchString(repository) || strings.Contains(repository, "..") {
		return nil, &agentHTTPError{status: http.StatusBadRequest, message: "name the plugin's repository as owner/name"}
	}
	if !pluginVersion.MatchString(version) {
		return nil, &agentHTTPError{status: http.StatusBadRequest, message: "the plugin version must look like 1, 1.2 or 1.2.3"}
	}
	var view struct {
		Arch string `json:"arch"`
	}
	if err := c.agentRequest(r, target, http.MethodGet, "/v1/plugins", nil, &view); err != nil {
		return nil, err
	}
	if view.Arch != "amd64" && view.Arch != "arm64" {
		return nil, errors.New("the server did not say which architecture it is")
	}
	base := "https://github.com/" + repository + "/releases/download/v" + version + "/"
	fetch := func(name string, limit int64, w io.Writer) (int64, error) {
		n, err := pluginReleaseFetch(r.Context(), base+name, limit, w)
		if err != nil && err.Error() == "not found" {
			return n, fmt.Errorf("release v%s of %s has no %s; a plugin release publishes plugin.json, plugin.sig and plugin-<architecture>.tar.gz", version, repository, name)
		}
		return n, err
	}
	var manifest, signature bytes.Buffer
	if _, err := fetch("plugin.json", plugin.MaxManifestSize, &manifest); err != nil {
		return nil, err
	}
	if _, err := fetch("plugin.sig", plugin.MaxManifestSize, &signature); err != nil {
		return nil, err
	}
	if !json.Valid(signature.Bytes()) {
		return nil, errors.New("the release's plugin.sig is not a signature document")
	}
	var staged struct {
		Name string `json:"name"`
	}
	if err := c.agentRequest(r, target, http.MethodPost, "/v1/plugins/manifest", map[string]any{"manifest": strings.TrimRight(manifest.String(), "\n"), "signature": json.RawMessage(signature.Bytes())}, &staged); err != nil {
		return nil, err
	}
	if !pluginRouteName.MatchString(staged.Name) {
		return nil, errors.New("the server did not name the plugin it verified")
	}
	archive, err := os.CreateTemp(c.cfg.DataDir, ".plugin-*.tar.gz")
	if err != nil {
		return nil, errors.New("the control panel could not store the download")
	}
	defer os.Remove(archive.Name())
	defer archive.Close()
	size, err := fetch("plugin-"+view.Arch+".tar.gz", plugin.MaxArchiveSize, archive)
	if err != nil {
		return nil, err
	}
	if _, err := archive.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	var record json.RawMessage
	if err := c.panelStream(r.Context(), target, http.MethodPut, "/v1/plugins/"+staged.Name+"/archive", "application/octet-stream", archive, size, 20*time.Minute, &record); err != nil {
		return nil, err
	}
	return record, nil
}

func (c *Control) pluginRoutes(handle func(string, http.HandlerFunc)) {
	for _, pattern := range []string{
		"GET /api/control/servers/{server}/plugins",
		"PUT /api/control/servers/{server}/plugins/keys",
		"POST /api/control/servers/{server}/plugins/manifest",
		"POST /api/control/servers/{server}/plugins/install",
		"PUT /api/control/servers/{server}/plugins/{name}/archive",
		"POST /api/control/servers/{server}/plugins/{name}/hooks/{hook}",
		"DELETE /api/control/servers/{server}/plugins/{name}",
	} {
		handle(pattern, c.serverPlugins)
	}
}

func (c *Control) serverPlugins(w http.ResponseWriter, r *http.Request) {
	target := c.serverByID(r.PathValue("server"))
	if target.ID == "" {
		http.NotFound(w, r)
		return
	}
	name, hook := r.PathValue("name"), r.PathValue("hook")
	if name != "" && !pluginRouteName.MatchString(name) {
		agent.Fail(w, 400, errors.New("invalid plugin name"))
		return
	}
	var out json.RawMessage
	// reply answers the agent's answer, or its error.
	reply := func(status int, err error) {
		if err != nil {
			// An agent without plugins answers 404 with no message of its own.
			var refused *agentHTTPError
			if errors.As(err, &refused) && refused.status == http.StatusNotFound && refused.message == "" {
				agent.Fail(w, http.StatusNotFound, errors.New("this server's agent is older than the control panel and has no plugins yet; update the agent on this server first"))
				return
			}
			failAgent(w, err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write(out)
	}
	if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/plugins/install") {
		var in struct {
			Repository string `json:"repository"`
			Version    string `json:"version"`
		}
		if err := agent.Decode(w, r, &in); err != nil {
			agent.Fail(w, 400, err)
			return
		}
		record, err := c.installPluginRelease(r, target, strings.TrimSpace(in.Repository), strings.TrimSpace(in.Version))
		if err != nil {
			var refused *agentHTTPError
			if errors.As(err, &refused) {
				reply(0, err)
				return
			}
			agent.Fail(w, http.StatusBadGateway, err)
			return
		}
		out = record
		reply(201, nil)
		return
	}
	switch {
	case r.Method == http.MethodGet:
		reply(200, c.agentRequest(r, target, http.MethodGet, "/v1/plugins", nil, &out))
	case r.Method == http.MethodPut && name == "":
		var in struct {
			Keys []string `json:"keys"`
		}
		if err := agent.Decode(w, r, &in); err != nil {
			agent.Fail(w, 400, err)
			return
		}
		reply(200, c.agentRequest(r, target, http.MethodPut, "/v1/plugins/keys", in, &out))
	case r.Method == http.MethodPost && hook == "":
		var in struct {
			Manifest  string          `json:"manifest"`
			Signature json.RawMessage `json:"signature"`
		}
		if err := agent.Decode(w, r, &in); err != nil {
			agent.Fail(w, 400, err)
			return
		}
		reply(200, c.agentRequest(r, target, http.MethodPost, "/v1/plugins/manifest", in, &out))
	case r.Method == http.MethodPut:
		if r.Header.Get("Content-Type") != "application/octet-stream" || r.ContentLength <= 0 || r.ContentLength > plugin.MaxArchiveSize {
			agent.Fail(w, 400, errors.New("upload the plugin archive as application/octet-stream with its length"))
			return
		}
		_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(20 * time.Minute))
		_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(21 * time.Minute))
		reply(201, c.panelStream(r.Context(), target, http.MethodPut, "/v1/plugins/"+name+"/archive", "application/octet-stream", http.MaxBytesReader(w, r.Body, r.ContentLength), r.ContentLength, 20*time.Minute, &out))
	case r.Method == http.MethodPost:
		if hook != "describe" && hook != "status" && hook != "check" {
			agent.Fail(w, 400, errors.New("only the describe, status and check hooks can be run on request"))
			return
		}
		var in json.RawMessage
		if err := agent.Decode(w, r, &in); err != nil {
			agent.Fail(w, 400, err)
			return
		}
		reply(200, c.agentRequest(r, target, http.MethodPost, "/v1/plugins/"+name+"/hooks/"+hook, in, &out))
	case r.Method == http.MethodDelete:
		reply(200, c.agentRequest(r, target, http.MethodDelete, "/v1/plugins/"+name, nil, &out))
	}
}
