package control

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/agim/lidza-deploy/internal/agent"
	"github.com/agim/lidza-deploy/internal/updates"
	"golang.org/x/mod/semver"
)

// Signed update channel: a distribution that pins bundle signing keys (see
// internal/updates/keys.go) publishes signed releases on its own repository.
// The control panel reads release metadata and assets with the GitHub App installation
// that already covers that repository, verifies the signed manifest, keeps a
// verified copy of the archive, and hands it to each agent, which verifies
// it again with the same pinned keys.

// UpdateChannel names the repository that publishes signed bundles.
type UpdateChannel struct {
	Repository   string `json:"repository"`
	Installation int64  `json:"installation"`
}

// UpdateBundle is the latest verified release the panel has seen.
type UpdateBundle struct {
	Version   string            `json:"version"`
	ReleaseID int64             `json:"release_id"`
	Manifest  string            `json:"manifest"`
	Signature updates.Signature `json:"signature"`
	Assets    map[string]int64  `json:"assets"`
	KeyID     string            `json:"key_id"`
}

var (
	channelTrust      = updates.Pinned
	repositoryPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
	// channelOwner gates channel changes to the installation owner; unit
	// tests without the framework's auth services replace it.
	channelOwner = func(c *Control, r *http.Request) bool { return c.githubOwner(r) }
)

func (c *Control) updateChannel() (UpdateChannel, bool) {
	v := c.updateSettingsSnapshot()
	if v.Channel == nil || !repositoryPattern.MatchString(v.Channel.Repository) || !channelTrust().Configured() {
		return UpdateChannel{}, false
	}
	return *v.Channel, true
}

func (c *Control) channelConfigured() bool {
	_, ok := c.updateChannel()
	return ok
}

// channelView is what the Updates page shows about the channel.
func (c *Control) channelView() map[string]any {
	v := c.updateSettingsSnapshot()
	trust := channelTrust()
	out := map[string]any{"configured": false, "keys": trust.Fingerprints(), "keys_pinned": trust.Configured()}
	if v.Channel != nil {
		out["repository"] = v.Channel.Repository
		out["installation"] = v.Channel.Installation
		out["configured"] = trust.Configured() && repositoryPattern.MatchString(v.Channel.Repository)
	}
	if v.Bundle != nil {
		out["bundle_version"] = v.Bundle.Version
		out["key_id"] = v.Bundle.KeyID
	}
	return out
}

// configureChannel binds the channel to a repository the connected GitHub
// App can read, or clears it. Stale release information is dropped so a
// public version is never offered after switching channels.
func (c *Control) configureChannel(ctx context.Context, repository string) error {
	v := c.updateSettingsSnapshot()
	if updateInProgress(v) {
		return errors.New("wait for the running update before changing the channel")
	}
	v.Latest, v.CheckError, v.Bundle, v.Version, v.Targets = "", "", nil, "", []UpdateTarget{}
	if repository == "" {
		v.Channel = nil
		return c.saveUpdateSettings(v)
	}
	if !repositoryPattern.MatchString(repository) {
		return errors.New("repository must be owner/name")
	}
	if !channelTrust().Configured() {
		return errors.New("this build pins no bundle signing key; the signed channel cannot verify releases")
	}
	app := c.githubApp()
	if app == nil || c.cfg.GitHub == nil {
		return errors.New("connect the GitHub App before configuring the signed update channel")
	}
	installation, err := c.cfg.GitHub.RepositoryInstallation(ctx, app.App, repository)
	if err != nil {
		return fmt.Errorf("the connected GitHub App cannot read %s: %w", repository, err)
	}
	if app.Installations[installation] == "" {
		return errors.New("the repository's GitHub App installation is not connected to this control panel")
	}
	v.Channel = &UpdateChannel{Repository: repository, Installation: installation}
	return c.saveUpdateSettings(v)
}

func (c *Control) channelToken(ctx context.Context, ch UpdateChannel) (string, error) {
	app := c.githubApp()
	if app == nil || c.cfg.GitHub == nil || app.Installations[ch.Installation] == "" {
		return "", errors.New("the signed update channel's GitHub installation is disconnected")
	}
	return c.cfg.GitHub.InstallationToken(ctx, app.App, ch.Installation, ch.Repository)
}

type releaseAsset struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
	Size int64  `json:"size"`
}
type release struct {
	ID         int64          `json:"id"`
	Tag        string         `json:"tag_name"`
	Draft      bool           `json:"draft"`
	Prerelease bool           `json:"prerelease"`
	Assets     []releaseAsset `json:"assets"`
}

// channelLatest finds the newest published channel release whose manifest
// verifies, records it, and returns its version.
func (c *Control) channelLatest(r *http.Request) (string, error) {
	ch, ok := c.updateChannel()
	if !ok {
		return "", errors.New("signed update channel is not configured")
	}
	ctx := r.Context()
	token, err := c.channelToken(ctx, ch)
	if err != nil {
		return "", err
	}
	var releases []release
	if err := c.cfg.GitHub.Request(ctx, token, "GET", "/repos/"+ch.Repository+"/releases?per_page=30", nil, &releases); err != nil {
		return "", fmt.Errorf("release list unavailable: %w", err)
	}
	var candidates []release
	for _, rel := range releases {
		if rel.Draft || rel.Prerelease || !updates.ValidVersion(rel.Tag) {
			continue
		}
		names := map[string]bool{}
		for _, a := range rel.Assets {
			names[a.Name] = true
		}
		if names[updates.ManifestName] && names[updates.SignatureName] && names[updates.ArchiveName("amd64")] {
			candidates = append(candidates, rel)
		}
	}
	if len(candidates) == 0 {
		return "", errors.New("no published channel release with a signed manifest")
	}
	slices.SortFunc(candidates, func(a, b release) int { return semver.Compare(b.Tag, a.Tag) })
	rel := candidates[0]
	assets := map[string]int64{}
	for _, a := range rel.Assets {
		assets[a.Name] = a.ID
	}
	var manifest, signature bytes.Buffer
	if err := c.downloadAsset(ctx, token, ch.Repository, assets[updates.ManifestName], updates.MaxManifestSize, &manifest); err != nil {
		return "", fmt.Errorf("manifest download failed: %w", err)
	}
	if err := c.downloadAsset(ctx, token, ch.Repository, assets[updates.SignatureName], updates.MaxManifestSize, &signature); err != nil {
		return "", fmt.Errorf("signature download failed: %w", err)
	}
	m, keyID, err := channelTrust().Verify(manifest.Bytes(), signature.Bytes())
	if err != nil {
		return "", fmt.Errorf("release %s refused: %w", rel.Tag, err)
	}
	if !strings.EqualFold(m.Repository, ch.Repository) || m.Version != rel.Tag {
		return "", fmt.Errorf("release %s refused: signed manifest names %s %s", rel.Tag, m.Repository, m.Version)
	}
	sig, _ := updates.ParseSignature(signature.Bytes())
	v := c.updateSettingsSnapshot()
	v.Bundle = &UpdateBundle{Version: m.Version, ReleaseID: rel.ID, Manifest: manifest.String(), Signature: sig, Assets: assets, KeyID: keyID}
	if err := c.saveUpdateSettings(v); err != nil {
		return "", err
	}
	return m.Version, nil
}

// downloadAsset streams one release asset. GitHub answers the authenticated
// API request with a redirect to a signed storage URL; that second request
// carries no credential.
func (c *Control) downloadAsset(ctx context.Context, token, repository string, id, limit int64, w io.Writer) error {
	if id <= 0 {
		return errors.New("release asset is missing")
	}
	api, err := url.Parse(c.cfg.GitHub.API)
	if err != nil {
		return err
	}
	if c.cfg.GitHub.API == "" {
		api, _ = url.Parse("https://api.github.com")
	}
	client := c.channelHTTP()
	first, err := http.NewRequestWithContext(ctx, "GET", fmt.Sprintf("%s/repos/%s/releases/assets/%d", strings.TrimSuffix(api.String(), "/"), repository, id), nil)
	if err != nil {
		return err
	}
	first.Header.Set("Authorization", "Bearer "+token)
	first.Header.Set("Accept", "application/octet-stream")
	first.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	res, err := client.Do(first)
	if err != nil {
		return errors.New("GitHub API unavailable")
	}
	defer res.Body.Close()
	if res.StatusCode == 302 || res.StatusCode == 301 || res.StatusCode == 307 {
		location, err := url.Parse(res.Header.Get("Location"))
		if err != nil || location.Host == "" || (location.Scheme != "https" && location.Host != api.Host) {
			return errors.New("release asset redirect is not acceptable")
		}
		second, err := http.NewRequestWithContext(ctx, "GET", location.String(), nil)
		if err != nil {
			return err
		}
		res.Body.Close()
		if res, err = client.Do(second); err != nil {
			return errors.New("release asset storage unavailable")
		}
		defer res.Body.Close()
	}
	if res.StatusCode != 200 {
		return fmt.Errorf("release asset returned %d", res.StatusCode)
	}
	n, err := io.Copy(w, io.LimitReader(res.Body, limit+1))
	if err != nil {
		return err
	}
	if n > limit {
		return errors.New("release asset exceeds its size limit")
	}
	return nil
}

func (c *Control) channelHTTP() *http.Client {
	if c.cfg.GitHub != nil && c.cfg.GitHub.HTTP != nil {
		return c.cfg.GitHub.HTTP
	}
	return &http.Client{Timeout: 10 * time.Minute, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

// cachedArchive returns the verified local copy of the bundle archive for
// arch, downloading it when missing. Other versions are removed.
func (c *Control) cachedArchive(ctx context.Context, bundle UpdateBundle, arch string) (string, error) {
	ch, ok := c.updateChannel()
	if !ok {
		return "", errors.New("signed update channel is not configured")
	}
	m, _, err := channelTrust().Verify([]byte(bundle.Manifest), mustJSON(bundle.Signature))
	if err != nil || m.Version != bundle.Version {
		return "", errors.New("recorded bundle manifest no longer verifies")
	}
	asset, err := m.Asset(arch)
	if err != nil {
		return "", err
	}
	root := filepath.Join(c.cfg.DataDir, "updates")
	dir := filepath.Join(root, m.Version)
	path := filepath.Join(dir, asset.Name)
	if _, err := updates.VerifyArchive(path, m, arch); err == nil {
		return path, nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	entries, _ := os.ReadDir(root)
	for _, entry := range entries {
		if entry.Name() != m.Version {
			os.RemoveAll(filepath.Join(root, entry.Name()))
		}
	}
	token, err := c.channelToken(ctx, ch)
	if err != nil {
		return "", err
	}
	file, err := os.CreateTemp(dir, ".download-")
	if err != nil {
		return "", err
	}
	defer os.Remove(file.Name())
	err = c.downloadAsset(ctx, token, ch.Repository, bundle.Assets[asset.Name], asset.Size, file)
	file.Close()
	if err != nil {
		return "", fmt.Errorf("archive download failed: %w", err)
	}
	if _, err := updates.VerifyArchive(file.Name(), m, arch); err != nil {
		return "", err
	}
	if err := os.Rename(file.Name(), path); err != nil {
		return "", err
	}
	return path, nil
}

// transferBundle stages the verified bundle on one agent: manifest and
// signature first, then the archive, each verified by the agent itself.
func (c *Control) transferBundle(r *http.Request, target Server, bundle UpdateBundle, arch string) error {
	if arch == "" {
		arch = "amd64"
	}
	path, err := c.cachedArchive(r.Context(), bundle, arch)
	if err != nil {
		return err
	}
	var staged struct {
		Version string `json:"version"`
		Arch    string `json:"arch"`
	}
	if err := c.agentRequest(r, target, "POST", "/v1/upgrade/manifest", map[string]any{"manifest": bundle.Manifest, "signature": bundle.Signature}, &staged); err != nil {
		return fmt.Errorf("agent refused the signed manifest: %w", err)
	}
	if staged.Arch != arch {
		return fmt.Errorf("agent architecture %s differs from the transferred archive", staged.Arch)
	}
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "PUT", target.URL+"/v1/upgrade/bundle/"+bundle.Version, file)
	if err != nil {
		return err
	}
	req.ContentLength = info.Size()
	req.Header.Set("Authorization", "Bearer "+target.Token)
	req.Header.Set("Content-Type", "application/octet-stream")
	client := &http.Client{Transport: c.client.Transport, Timeout: 20 * time.Minute}
	res, err := client.Do(req)
	if err != nil {
		return errors.New("agent unavailable during bundle transfer")
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		var e struct {
			Error string `json:"error"`
		}
		_ = json.NewDecoder(io.LimitReader(res.Body, 4096)).Decode(&e)
		return fmt.Errorf("agent refused the bundle archive: %s", e.Error)
	}
	return nil
}

func mustJSON(v any) []byte {
	raw, _ := json.Marshal(v)
	return raw
}

// agentArch reads the architecture an agent reported, defaulting to amd64.
func agentArch(status agent.UpgradeStatus) string {
	if status.Arch == "arm64" {
		return "arm64"
	}
	return "amd64"
}

// updateChannelRoute handles POST /api/control/updates/channel: the
// installation owner binds or clears the signed channel repository.
func (c *Control) updateChannelRoute(w http.ResponseWriter, r *http.Request) {
	if !channelOwner(c, r) {
		agent.Fail(w, 403, errors.New("only the installation owner can change the update channel"))
		return
	}
	var in struct {
		Repository string `json:"repository"`
	}
	if err := agent.Decode(w, r, &in); err != nil {
		agent.Fail(w, 400, err)
		return
	}
	if err := c.configureChannel(r.Context(), strings.TrimSpace(in.Repository)); err != nil {
		agent.Fail(w, 409, err)
		return
	}
	if in.Repository != "" {
		_ = c.checkUpdateRelease(r) // the result, including an error, is shown in settings
	}
	agent.JSON(w, 200, map[string]any{"settings": c.updateSettingsSnapshot(), "channel": c.channelView()})
}
