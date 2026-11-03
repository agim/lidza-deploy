package control

// Offline control-instance staging preserves opaque product state. It never
// starts a control process, migrates a database or changes the source directory.
import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/agim/lidza-deploy/internal/agent"
	"github.com/agim/lidza/pkg/credentials"
)

type HandoffEndpoint struct {
	ID   string `json:"id"`
	From string `json:"from"`
	To   string `json:"to"`
}
type HandoffRequest struct {
	Origin    string            `json:"origin"`
	Endpoints []HandoffEndpoint `json:"endpoints"`
}
type HandoffReview struct {
	SourceSHA256      string            `json:"source_sha256"`
	Origin            string            `json:"origin"`
	ApplicationCount  int               `json:"application_count"`
	FileCount         int               `json:"file_count"`
	Endpoints         []HandoffEndpoint `json:"endpoints"`
	EndpointsVerified bool              `json:"endpoints_verified"`
	DatabaseIncluded  bool              `json:"database_included"`
	ReadyToStart      bool              `json:"ready_to_start"`
	Warnings          []string          `json:"warnings"`
}

func readHandoffFiles(dir string) (map[string][]byte, string, error) {
	if !filepath.IsAbs(dir) {
		return nil, "", errors.New("source directory must be absolute")
	}
	real, err := filepath.EvalSymlinks(dir)
	if err != nil || real != filepath.Clean(dir) {
		return nil, "", errors.New("source directory must exist without a symlink")
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, "", errors.New("source directory unavailable")
	}
	defer root.Close()
	files := map[string][]byte{}
	total := int64(0)
	err = fs.WalkDir(root.FS(), ".", func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return errors.New("control files unavailable")
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return errors.New("control snapshot refuses symlinks")
		}
		if entry.IsDir() {
			return nil
		}
		if !entry.Type().IsRegular() || len(files) >= 4096 {
			return errors.New("control snapshot requires bounded regular files")
		}
		f, e := root.Open(filepath.FromSlash(path))
		if e != nil {
			return errors.New("control file unavailable")
		}
		defer f.Close()
		data, e := io.ReadAll(io.LimitReader(f, 64<<20+1))
		total += int64(len(data))
		if e != nil || len(data) > 64<<20 || total > 256<<20 {
			return errors.New("control snapshot exceeds file or total size limit")
		}
		files[path] = data
		return nil
	})
	if err != nil {
		return nil, "", err
	}
	hash := sha256.New()
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		sum := sha256.Sum256(files[name])
		hash.Write([]byte(name))
		hash.Write([]byte{0})
		hash.Write(sum[:])
	}
	return files, hex.EncodeToString(hash.Sum(nil)), nil
}

func prepareControlHandoff(files map[string][]byte, request HandoffRequest) (HandoffReview, map[string][]byte, []Server, map[string]Application, error) {
	review := HandoffReview{Origin: request.Origin, FileCount: len(files), Endpoints: slices.Clone(request.Endpoints), Warnings: []string{"Database contents are not included. Restore and verify the dashboard database separately.", "Stop and drain the source control writer/dispatcher before the final snapshot. Start only one destination writer.", "Preserve the public origin, authentication keys, OAuth/GitHub identity and webhook delivery state.", "Review service environment overrides, DNS/HTTPS routing and current-state rollback before starting."}}
	fail := func() (HandoffReview, map[string][]byte, []Server, map[string]Application, error) {
		return review, nil, nil, nil, errors.New("retained control state or handoff mapping is invalid")
	}
	if os.Getenv(credentials.EnvMasterKey) != "" || os.Getenv(credentials.EnvKeyOff) == "1" {
		return review, nil, nil, nil, errors.New("offline staging requires the retained file key with no master-key environment override")
	}
	key, e := hex.DecodeString(strings.TrimSpace(string(files[credentials.MasterKeyFile])))
	if e != nil || len(key) != 32 {
		return fail()
	}
	sealed := strings.TrimSpace(string(files[credentials.File]))
	plain, e := credentials.Decrypt(key, sealed)
	if e != nil {
		return fail()
	}
	values, e := credentials.Parse(string(plain))
	if e != nil {
		return fail()
	}
	origin := values["production.PUBLIC_URL"]
	if origin == "" {
		origin = values["PUBLIC_URL"]
	}
	if origin == "" || origin != request.Origin || len(files["setup-complete.json"]) == 0 {
		return fail()
	}
	u, e := url.Parse(origin)
	if e != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") || (u.Scheme != "https" && !(u.Scheme == "http" && (u.Hostname() == "127.0.0.1" || u.Hostname() == "localhost"))) {
		return fail()
	}
	var encrypted string
	if json.Unmarshal(files["control.enc.json"], &encrypted) != nil {
		return fail()
	}
	controlPlain, e := credentials.Decrypt(key, encrypted)
	if e != nil {
		return fail()
	}
	var raw map[string]json.RawMessage
	if json.Unmarshal(controlPlain, &raw) != nil || raw == nil {
		return fail()
	}
	var rawServers []map[string]json.RawMessage
	var servers []Server
	var apps map[string]Application
	if json.Unmarshal(raw["servers"], &servers) != nil || validateServers(servers) != nil || json.Unmarshal(raw["servers"], &rawServers) != nil || json.Unmarshal(raw["apps"], &apps) != nil || apps == nil {
		return fail()
	}
	if len(request.Endpoints) != len(servers) {
		return fail()
	}
	byID := map[string]HandoffEndpoint{}
	for _, mapping := range request.Endpoints {
		if _, ok := byID[mapping.ID]; ok {
			return fail()
		}
		byID[mapping.ID] = mapping
	}
	for i, s := range servers {
		mapping, ok := byID[s.ID]
		if !ok || mapping.From != s.URL {
			return fail()
		}
		servers[i].URL = mapping.To
		rawServers[i]["url"], _ = json.Marshal(mapping.To)
	}
	if validateServers(servers) != nil {
		return fail()
	}
	for id, a := range apps {
		if id != a.ID {
			return fail()
		}
		if _, ok := byID[a.ServerID]; !ok {
			return fail()
		}
	}
	review.ApplicationCount = len(apps)
	raw["servers"], _ = json.Marshal(rawServers)
	// Retain unknown/private fields, including native bindings and future schema.
	controlPlain, e = json.Marshal(raw)
	if e != nil {
		return fail()
	}
	encrypted, e = credentials.Encrypt(key, controlPlain)
	if e != nil {
		return fail()
	}
	out := make(map[string][]byte, len(files))
	for name, data := range files {
		out[name] = bytes.Clone(data)
	}
	out["control.enc.json"], _ = json.MarshalIndent(encrypted, "", "  ")
	// The boot seed must match the remapped encrypted registry, not old loopback.
	if _, exists := out["servers.json"]; exists {
		out["servers.json"], _ = json.MarshalIndent(rawServers, "", "  ")
	}
	return review, out, servers, apps, nil
}

// ReviewControlHandoff never writes or contacts an agent. URLs are operator
// mapping inputs; no tokens, credentials or database connections are returned.
func ReviewControlHandoff(dir string, request HandoffRequest) (HandoffReview, error) {
	files, digest, err := readHandoffFiles(dir)
	if err != nil {
		return HandoffReview{}, err
	}
	review, _, _, _, err := prepareControlHandoff(files, request)
	review.SourceSHA256 = digest
	return review, err
}

// StageControlHandoff writes only a new offline directory after checking the
// reviewed source digest and each destination agent's registered applications.
// The source must be stopped for the final snapshot; staging does not stop it.
func StageControlHandoff(ctx context.Context, dir, output, expected string, request HandoffRequest) (HandoffReview, error) {
	files, digest, err := readHandoffFiles(dir)
	if err != nil {
		return HandoffReview{}, err
	}
	if expected == "" || expected != digest {
		return HandoffReview{}, errors.New("source changed since review; review a fresh snapshot")
	}
	review, out, servers, apps, err := prepareControlHandoff(files, request)
	review.SourceSHA256 = digest
	if err != nil {
		return review, err
	}
	c := &Control{client: &http.Client{Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
	for _, server := range servers {
		req, _ := http.NewRequestWithContext(ctx, "GET", request.Origin, nil)
		var remote []agent.App
		if err := c.agentRequest(req, server, "GET", "/v1/apps", nil, &remote); err != nil {
			return review, errors.New("destination agent could not be verified")
		}
		for _, a := range apps {
			if a.ServerID != server.ID {
				continue
			}
			found := false
			for _, b := range remote {
				if a.ID == b.ID && a.Repository == b.Repository && a.Domain == b.Domain && slices.Equal(a.AdditionalDomains, b.AdditionalDomains) {
					found = true
					break
				}
			}
			if !found {
				return review, errors.New("destination agent is missing a matching registered application")
			}
		}
	}
	review.EndpointsVerified = true
	_, fresh, err := readHandoffFiles(dir)
	if err != nil || fresh != digest {
		return review, errors.New("source changed during staging; stop the writer and review again")
	}
	if !filepath.IsAbs(output) {
		return review, errors.New("output must be a new absolute directory")
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(output))
	if err != nil {
		return review, errors.New("staging parent directory unavailable")
	}
	resolved := filepath.Join(parent, filepath.Base(output))
	rel, err := filepath.Rel(filepath.Clean(dir), resolved)
	if err != nil || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))) {
		return review, errors.New("staging output must be outside the source directory")
	}
	if err := os.Mkdir(output, 0700); err != nil {
		return review, errors.New("output must not already exist and its parent must exist")
	}
	complete := false
	defer func() {
		if !complete {
			_ = os.RemoveAll(output)
		}
	}()
	for name, data := range out {
		path := filepath.Join(output, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			return review, err
		}
		if err := os.WriteFile(path, data, 0600); err != nil {
			return review, err
		}
	}
	manifest, _ := json.MarshalIndent(review, "", "  ")
	if err := os.WriteFile(filepath.Join(output, "handoff-review.json"), manifest, 0600); err != nil {
		return review, err
	}
	complete = true
	return review, nil
}
