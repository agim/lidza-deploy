package agent

import (
	"context"
	"errors"
	"io"
	"maps"
	"slices"
	"strings"

	"github.com/agim/lidza/pkg/config"
)

const appStorageMount = "/var/lib/lidza-storage"

// The local framework provider is suitable for this single-server deployment
// only when its files live outside release containers on a lasting disk.
func (m *Manager) prepareAppStorage(ctx context.Context, a App, cfg *config.Config, effective map[string]string) (App, error) {
	if !slices.Contains(cfg.Packs, "lidza/storage") {
		return a, nil
	}
	provider := strings.ToLower(strings.TrimSpace(effective["STORAGE_PROVIDER"]))
	if provider != "" && provider != "local" {
		return a, nil
	}
	m.mu.Lock()
	original, ok := m.data.Apps[a.ID]
	if !ok || original.Retiring {
		m.mu.Unlock()
		return a, errors.New("application unavailable")
	}
	volume := m.data.StorageVolumes[a.ID]
	if volume == "" {
		volume = "lidza-storage-" + a.ID
		m.data.StorageVolumes[a.ID] = volume
		if err := m.save(); err != nil {
			delete(m.data.StorageVolumes, a.ID)
			m.mu.Unlock()
			return a, errors.New("could not save local storage identity")
		}
	}
	m.mu.Unlock()
	if err := prepareStorageVolume(ctx, a.ID, volume); err != nil {
		return a, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	original = m.data.Apps[a.ID]
	if original.ID == "" || original.Retiring {
		return a, errors.New("application unavailable")
	}
	updated := original
	updated.Env = maps.Clone(original.Env)
	if updated.Env == nil {
		updated.Env = map[string]string{}
	}
	updated.Env["STORAGE_PROVIDER"] = "local"
	updated.Env["STORAGE_DIR"] = appStorageMount
	if err := updated.Validate(); err != nil {
		return a, err
	}
	m.data.Apps[a.ID] = updated
	if err := m.save(); err != nil {
		m.data.Apps[a.ID] = original
		return a, errors.New("could not save persistent storage attachment")
	}
	a.Env = maps.Clone(updated.Env)
	a.StorageVolume = volume
	effective["STORAGE_PROVIDER"] = "local"
	effective["STORAGE_DIR"] = appStorageMount
	return a, nil
}

func prepareStorageVolume(ctx context.Context, id, volume string) error {
	if volume != "lidza-storage-"+id || !idPattern.MatchString(id) {
		return errors.New("invalid managed storage identity")
	}
	found, err := command(ctx, "", nil, "docker", "volume", "ls", "--filter", "name=^"+volume+"$", "--format", "{{.Name}}")
	if err != nil {
		return errors.New("could not inspect persistent storage")
	}
	if found == "" {
		if _, err = command(ctx, "", nil, "docker", "volume", "create", "--label", "io.lidza.storage="+id, volume); err != nil {
			return errors.New("could not create persistent storage")
		}
	} else {
		owner, e := command(ctx, "", nil, "docker", "volume", "inspect", "--format", `{{index .Labels "io.lidza.storage"}}`, volume)
		if e != nil || owner != id {
			return errors.New("persistent storage name is occupied by an unmanaged volume")
		}
	}
	// Initialize only the mount root. Never recursively change user uploads.
	err = dockerStream(ctx, nil, io.Discard, nil, "run", "--rm", "--network", "none", "--user", "0:0", "--read-only", "--cap-drop", "ALL", "--cap-add", "CHOWN", "--cap-add", "FOWNER", "--security-opt", "no-new-privileges", "--memory", "64m", "--pids-limit", "32", "--mount", "type=volume,src="+volume+",dst=/data,volume-nocopy", cacheImage, "sh", "-c", "chown 65532:65532 /data && chmod 700 /data")
	if err != nil {
		return errors.New("could not initialize persistent storage permissions")
	}
	return nil
}

func storageMountArgs(a App) []string {
	if a.StorageVolume == "" || a.Env["STORAGE_PROVIDER"] != "local" {
		return nil
	}
	return []string{"--mount", "type=volume,src=" + a.StorageVolume + ",dst=" + appStorageMount + ",volume-nocopy", "--env", "STORAGE_DIR=" + appStorageMount}
}
