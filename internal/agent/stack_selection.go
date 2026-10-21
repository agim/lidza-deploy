package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"path"
	"slices"
	"strconv"
	"strings"
)

func validateSelection(s *StackSelectionSnapshot, c *StackRuntimeSelection) error {
	if s == nil || s.Version != 1 || !configurationText(s.Revision, 128) || strings.TrimSpace(s.Revision) == "" || len(s.Values) != len(c.Selectors) {
		return errors.New("invalid runtime selection snapshot/version/revision")
	}
	for key, allowed := range c.Selectors {
		if !slices.Contains(allowed, s.Values[key]) {
			return fmt.Errorf("runtime selector %s has a missing or unsupported value", key)
		}
	}
	return nil
}
func readStackSelection(ctx context.Context, a App, c *StackRuntimeSelection) (*StackSelectionSnapshot, error) {
	if a.Current == nil {
		return nil, errors.New("runtime selection needs a running release")
	}
	port, err := strconv.Atoi(a.Current.Port)
	if err != nil || port < 1 || port > 65535 {
		return nil, errors.New("current app private address is unavailable; recover the recorded release before changing selections")
	}
	tokenKey, endpoint := c.TokenKey, c.Path
	if a.Current.Stack != nil && a.Current.Stack.Manifest.RuntimeSelection != nil {
		previous := a.Current.Stack.Manifest.RuntimeSelection
		if previous.TokenKey != tokenKey {
			return nil, errors.New("runtime selection token identity cannot change during redeploy")
		}
		endpoint = previous.Path
	}
	token := a.Env[tokenKey]
	if token == "" {
		return nil, errors.New("runtime selection exchange credential is unavailable")
	}
	req, err := http.NewRequestWithContext(ctx, "GET", "http://127.0.0.1:"+strconv.Itoa(port)+endpoint, nil)
	if err != nil {
		return nil, errors.New("invalid runtime selection request")
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Cache-Control", "no-store")
	res, err := customerHostClient.Do(req)
	if err != nil {
		return nil, errors.New("private runtime selection endpoint unavailable; current release retained, no defaults substituted")
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		return nil, fmt.Errorf("private runtime selection endpoint returned HTTP %d; verify the app exposes its authenticated endpoint and retained token before migrating/redeploying", res.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(res.Body, 16385))
	if err != nil || len(data) > 16384 {
		return nil, errors.New("runtime selection response exceeds 16 KiB or could not be read")
	}
	var snapshot StackSelectionSnapshot
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if dec.Decode(&snapshot) != nil || dec.Decode(&struct{}{}) != io.EOF {
		return nil, errors.New("invalid runtime selection response; expected one snapshot JSON object")
	}
	if err = validateSelection(&snapshot, c); err != nil {
		return nil, err
	}
	return &snapshot, nil
}
func resolveStackSelection(ctx context.Context, a App, m StackManifest) (*StackSelectionSnapshot, error) {
	c := m.RuntimeSelection
	if c == nil {
		return nil, nil
	}
	var selected *StackSelectionSnapshot
	var err error
	switch {
	case a.Current == nil:
		selected = &StackSelectionSnapshot{Version: 1, Revision: "initial", Values: maps.Clone(c.Initial)}
		for name := range c.Selectors {
			if value, provided := a.Env[name]; provided {
				selected.Values[name] = value
			}
		}
	case a.UseRecordedSelection:
		if a.Current.Stack != nil {
			selected = cloneStackSelection(a.Current.Stack.Selection)
		}
		if err = validateSelection(selected, c); err != nil {
			return nil, errors.New("start requires a compatible recorded runtime selection; deploy an explicitly configured recovery instead")
		}
	default:
		selected, err = readStackSelection(ctx, a, c)
		if err != nil {
			return nil, err
		}
	}
	if err = validateSelection(selected, c); err != nil {
		return nil, err
	}
	for key, value := range selected.Values {
		if configured, ok := a.Env[key]; ok && configured != value {
			return nil, fmt.Errorf("operator setting %s conflicts with the app-published runtime choice; resolve the explicit conflict before deploying", key)
		}
	}
	return selected, nil
}
func (d *Docker) prepareStackPlan(ctx context.Context, a App, m StackManifest) (App, error) {
	if err := m.validate(); err != nil {
		return a, err
	}
	if m.Version != 2 {
		return a, nil
	}
	var err error
	if a.StackSelection == nil && m.RuntimeSelection != nil {
		a.StackSelection, err = resolveStackSelection(ctx, a, m)
		if err != nil {
			return a, err
		}
	}
	if err = m.validatePlan(a, a.StackSelection); err != nil {
		return a, err
	}
	a.Env = maps.Clone(a.Env)
	if a.Env == nil {
		a.Env = map[string]string{}
	}
	if a.StackSelection != nil {
		maps.Copy(a.Env, a.StackSelection.Values)
	}
	return a, nil
}
func verifyStackSelection(ctx context.Context, a App, m StackManifest) error {
	if m.RuntimeSelection == nil || a.Current == nil || a.UseRecordedSelection {
		return nil
	}
	now, err := readStackSelection(ctx, a, m.RuntimeSelection)
	if err != nil {
		return err
	}
	if a.StackSelection == nil || now.Revision != a.StackSelection.Revision || !maps.Equal(now.Values, a.StackSelection.Values) {
		return errors.New("app runtime selection changed while preparing deployment; current writers retained, retry with a fresh plan")
	}
	return nil
}
func privateRuntimeSelectionPath(a App, p string) bool {
	cleaned := path.Clean(p)
	for _, r := range []*Release{a.Current, a.Previous} {
		if r == nil || r.Stack == nil || r.Stack.Manifest.RuntimeSelection == nil {
			continue
		}
		private := r.Stack.Manifest.RuntimeSelection.Path
		if cleaned == private || strings.HasPrefix(cleaned, private+"/") {
			return true
		}
	}
	return false
}

func verifyCandidateSelection(ctx context.Context, a App, r *Release) error {
	c := r.Stack.Manifest.RuntimeSelection
	if c == nil {
		return nil
	}
	candidate := a
	candidate.Current = r
	snapshot, err := readStackSelection(ctx, candidate, c)
	if err != nil {
		return err
	}
	if r.Stack.Selection == nil || !maps.Equal(snapshot.Values, r.Stack.Selection.Values) {
		return errors.New("candidate requires a different service topology; refusing activation, retry after reconciling saved desired configuration")
	}
	if a.Current != nil && !a.UseRecordedSelection && snapshot.Revision != r.Stack.Selection.Revision {
		return errors.New("runtime selection revision changed during activation; recovering the previous stack")
	}
	r.Stack.Selection = cloneStackSelection(snapshot)
	return nil
}
func validateRollbackSelection(ctx context.Context, a App) error {
	previous := a.Previous.Stack
	var current *StackRuntimeSelection
	if a.Current != nil && a.Current.Stack != nil {
		current = a.Current.Stack.Manifest.RuntimeSelection
	}
	if previous.Manifest.RuntimeSelection == nil {
		if current != nil {
			return errors.New("rollback to a release without runtime-selection support requires an explicit app migration; current topology retained")
		}
		return nil
	}
	desired, err := readStackSelection(ctx, a, previous.Manifest.RuntimeSelection)
	if err != nil {
		return err
	}
	if previous.Selection == nil || !maps.Equal(desired.Values, previous.Selection.Values) {
		return errors.New("previous release topology conflicts with active/saved desired configuration; publish a compatible selection or deploy a reconciled release before rollback")
	}
	return nil
}
