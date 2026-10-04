package agent

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"time"
)

func (m *Manager) removePreview(ctx context.Context, id string) error {
	m.mu.Lock()
	a, exists := m.data.Apps[id]
	if exists && !a.Preview {
		m.mu.Unlock()
		return errors.New("not a preview app")
	}
	m.mu.Unlock()
	if exists {
		if err := m.Retire(ctx, id); err != nil {
			return err
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for dbID, d := range m.data.Databases {
		if !d.Ephemeral || d.PreviewID != id {
			continue
		}
		if d.Operation != "" || len(m.attachmentsLocked(dbID)) > 0 {
			return errors.New("preview database still busy or attached")
		}
		for _, app := range m.data.Apps {
			for _, release := range []*Release{app.Current, app.Previous} {
				if release != nil {
					for _, used := range release.DatabaseIDs {
						if used == dbID {
							return errors.New("preview database still used by another release")
						}
					}
				}
			}
		}
		cleanup, cancel := context.WithTimeout(ctx, 20*time.Second)
		var err error
		for _, resource := range []struct{ kind, name string }{{"container", d.Network}, {"volume", d.Network + "-data"}, {"network", d.Network}} {
			var found string
			switch resource.kind {
			case "container":
				found, err = command(cleanup, "", nil, "docker", "ps", "-a", "--filter", "name=^/"+resource.name+"$", "--format", "{{.ID}}")
			default:
				found, err = command(cleanup, "", nil, "docker", resource.kind, "ls", "--filter", "name=^"+resource.name+"$", "--format", "{{.Name}}")
			}
			if err != nil {
				break
			}
			if found == "" {
				continue
			}
			if resource.kind == "container" {
				_, err = command(cleanup, "", nil, "docker", "rm", "-f", resource.name)
			} else {
				_, err = command(cleanup, "", nil, "docker", resource.kind, "rm", resource.name)
			}
			if err != nil {
				break
			}
		}
		cancel()
		if err != nil {
			return fmt.Errorf("preview database cleanup failed; retry: %w", err)
		}
		if err = os.RemoveAll(m.backupDir(dbID)); err != nil {
			return errors.New("preview backup cleanup failed; retry")
		}

		delete(m.data.Databases, dbID)
	}
	return m.save()
}
func (m *Manager) previewRoute(w http.ResponseWriter, r *http.Request) {
	if err := m.removePreview(r.Context(), r.PathValue("id")); err != nil {
		Fail(w, 409, err)
		return
	}
	JSON(w, 200, map[string]string{"status": "removed"})
}
