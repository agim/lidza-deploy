package agent

// Backups of a plugin's service (docs/plugins-proposal.md): a plugin that
// implements the backup hook gets a protected resource for each application
// it serves, so its data is in the same pipeline as every database: the
// schedule, retention, the off-site copy and the backup before a release.
// The agent gives the hook an empty directory and archives what it leaves
// there; a restore hands the same files back.

import (
	"archive/tar"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

func nativeHostPluginResourceID(id string) string {
	base := strings.TrimRight(id[:min(len(id), 37)], "-")
	var b [4]byte
	_, _ = rand.Read(b[:])
	return fmt.Sprintf("%s-pl%x", base, b[:])
}

// nativePluginResourceURL identifies the resource; it carries no secret.
func nativePluginResourceURL(service, app string) string {
	return "plugin://" + service + "/" + app
}

// nativeHostPluginDigest is the binding's configuration identity.
func nativeHostPluginDigest(service, app, resource string) string {
	sum := sha256.Sum256([]byte("lidza-host-plugin\x00" + service + "\x00" + app + "\x00" + resource))
	return hex.EncodeToString(sum[:])
}

// validateNativePluginPolicy checks the exact binding of a plugin service
// the agent provisioned. It has no dump tool and no connection: the plugin
// backs its service up.
func validateNativePluginPolicy(id string, p NativeDatabasePolicy) error {
	if !idPattern.MatchString(id) || !p.Enabled || !idPattern.MatchString(p.AppID) || !pluginServiceName.MatchString(p.PluginService) ||
		p.ConfigSHA256 != nativeHostPluginDigest(p.PluginService, p.AppID, id) || p.DumpSHA256 != "" || p.Host != "" || p.Port != "" || p.MaintenancePort != "" ||
		p.EnvironmentKey != "" || len(p.Roles) != 0 || p.SQLitePath != "" || p.ArchiveDatabase != "" || p.ClickHouseDatabase != "" || p.PostgresMajor != 0 {
		return errors.New("a plugin's service requires its exact provisioned policy")
	}
	if p.Backup.Keep != 1 {
		return errors.New("native database keeps exactly one finalized local backup")
	}
	return p.Backup.Validate()
}

// protectPluginService registers the protected resource of a plugin service
// that can be backed up, and answers its id.
func (m *Manager) protectPluginService(id, service string, backup BackupPolicy) (string, error) {
	record, _ := m.hostDataRecord(id)
	resource := record.Plugins[service].Resource
	if resource == "" {
		resource = nativeHostPluginResourceID(id)
		if err := m.updateHostDataRecord(id, func(r *NativeHostDataRecord) {
			entry := r.Plugins[service]
			entry.Resource = resource
			r.Plugins[service] = entry
		}); err != nil {
			return "", err
		}
	}
	policy := NativeDatabasePolicy{Enabled: true, AppID: id, PluginService: service, ConfigSHA256: nativeHostPluginDigest(service, id, resource), Backup: backup}
	if err := validateNativeDatabasePolicy(resource, policy); err != nil {
		return "", err
	}
	return resource, m.registerHostResource(id, resource, nativePluginResourceURL(service, id), policy, func(r *NativeHostDataRecord) bool {
		return r.Plugins[service].Resource == resource
	})
}

// pluginResourceApp returns the application and service a protected
// resource belongs to.
func (m *Manager) pluginResourceApp(resource string) (app, service string, ok bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p := m.data.NativeDatabases[resource].Policy
	if p.PluginService == "" || m.data.NativeHostData[p.AppID].Plugins[p.PluginService].Resource != resource {
		return "", "", false
	}
	return p.AppID, p.PluginService, true
}

// pluginServiceInput is the input of a hook for a provisioned service.
func (m *Manager) pluginServiceInput(app, service, resource string) (NativeHostPlugin, json.RawMessage, error) {
	record, _ := m.hostDataRecord(app)
	entry, ok := record.Plugins[service]
	if !ok || entry.Resource != resource {
		return entry, nil, errors.New("the plugin service's binding changed")
	}
	secrets, err := m.pluginSecrets(entry.Secrets)
	if err != nil {
		return entry, nil, err
	}
	raw, err := json.Marshal(pluginAppInput{App: app, Record: entry.Record, Secrets: secrets})
	return entry, raw, err
}

// dumpPluginService writes a backup of a plugin's service: what the backup
// hook left in its directory, as one tar (Zstandard when the tool is
// installed).
func (m *Manager) dumpPluginService(ctx context.Context, d Database, p NativeDatabasePolicy, output io.Writer) error {
	entry, input, err := m.pluginServiceInput(p.AppID, p.PluginService, d.AppID)
	if err != nil {
		return fmt.Errorf("%w; backup refused", err)
	}
	work, err := os.MkdirTemp(m.backupDir(d.AppID), ".plugin-*")
	if err != nil {
		return errors.New("cannot create the plugin's backup directory")
	}
	defer os.RemoveAll(work)
	result, err := m.runPluginHookWork(ctx, entry.Plugin, "backup", input, work)
	if err != nil {
		return err
	}
	if !result.Answer.OK {
		return fmt.Errorf("the %s plugin could not back up the service: %s", entry.Plugin, cmpMessages(result.Answer.Messages))
	}
	compression := compressionNone
	if zstdAvailable() {
		compression = compressionZstd
	}
	compressing, cancel := context.WithCancel(ctx)
	defer cancel()
	writer, err := compressedWriter(compressing, output, compression)
	if err != nil {
		return err
	}
	err = tarPluginDirectory(work, writer)
	if closeErr := writer.Close(); err == nil {
		err = closeErr
	}
	return err
}

// tarPluginDirectory archives the regular files and directories of dir. A
// plugin leaves nothing else in a backup; anything else is refused.
func tarPluginDirectory(dir string, w io.Writer) error {
	tw := tar.NewWriter(w)
	files := 0
	err := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, path)
		if rel == "." {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		switch {
		case info.IsDir():
			return tw.WriteHeader(&tar.Header{Name: filepath.ToSlash(rel) + "/", Mode: 0o700, Typeflag: tar.TypeDir, ModTime: info.ModTime()})
		case !info.Mode().IsRegular():
			return fmt.Errorf("the plugin left %s in its backup, which is not a regular file", rel)
		}
		file, err := os.Open(path)
		if err != nil {
			return err
		}
		defer file.Close()
		if err := tw.WriteHeader(&tar.Header{Name: filepath.ToSlash(rel), Mode: 0o600, Size: info.Size(), Typeflag: tar.TypeReg, ModTime: info.ModTime()}); err != nil {
			return err
		}
		files++
		_, err = io.Copy(tw, file)
		return err
	})
	if err != nil {
		return err
	}
	if files == 0 {
		return errors.New("the plugin's backup hook left no files")
	}
	return tw.Close()
}

// untarPluginDirectory unpacks an archive tarPluginDirectory wrote.
func untarPluginDirectory(r io.Reader, dir string) error {
	tr := tar.NewReader(r)
	for {
		header, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return errors.New("the backup archive cannot be read")
		}
		name := filepath.Clean(filepath.FromSlash(header.Name))
		if name == "." || filepath.IsAbs(name) || name == ".." || strings.HasPrefix(name, ".."+string(filepath.Separator)) {
			return errors.New("the backup archive holds a path outside its directory")
		}
		target := filepath.Join(dir, name)
		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o700); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
				return err
			}
			file, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
			if err != nil {
				return err
			}
			_, err = io.Copy(file, tr)
			if closeErr := file.Close(); err == nil {
				err = closeErr
			}
			if err != nil {
				return err
			}
		default:
			return errors.New("the backup archive holds something other than files and directories")
		}
	}
}

// restorePluginBackup verifies a backup and hands its files to the plugin's
// restore hook, which replaces the service's data for the application.
func (m *Manager) restorePluginBackup(ctx context.Context, app, resource string, record BackupRecord) error {
	progress := progressFrom(ctx)
	file, err := os.Open(filepath.Join(m.backupDir(resource), record.ID+".dump"))
	if err != nil {
		return errors.New("local backup unavailable")
	}
	defer file.Close()
	hash := sha256.New()
	verified := &progressReader{r: file}
	progress.Stage("Verifying the backup's SHA-256", record.Size, verified.n.Load)
	if n, err := io.Copy(hash, verified); err != nil || n != record.Size || hex.EncodeToString(hash.Sum(nil)) != record.SHA256 {
		return errors.New("backup integrity check failed; the service was left unchanged")
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return err
	}
	return m.restorePluginArchive(ctx, app, resource, file)
}

// restorePluginArchive unpacks a verified backup archive and runs the
// plugin's restore hook on its files.
func (m *Manager) restorePluginArchive(ctx context.Context, app, resource string, file io.Reader) error {
	progress := progressFrom(ctx)
	m.mu.Lock()
	service := m.data.NativeDatabases[resource].Policy.PluginService
	m.mu.Unlock()
	entry, input, err := m.pluginServiceInput(app, service, resource)
	if err != nil {
		return err
	}
	work, err := os.MkdirTemp(m.backupDir(resource), ".plugin-*")
	if err != nil {
		return errors.New("cannot create the plugin's restore directory")
	}
	defer os.RemoveAll(work)
	progress.Stage("Unpacking the backup", 0, nil)
	reader, _, err := decompressedReader(ctx, file)
	if err != nil {
		return err
	}
	err = untarPluginDirectory(reader, work)
	if closeErr := reader.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	progress.Stage("Restoring through the "+entry.Plugin+" plugin", 0, nil)
	result, err := m.runPluginHookWork(ctx, entry.Plugin, "restore", input, work)
	if err != nil {
		return err
	}
	if !result.Answer.OK {
		return fmt.Errorf("the %s plugin could not restore the backup: %s", entry.Plugin, cmpMessages(result.Answer.Messages))
	}
	return nil
}
