// Command plugin-example is a complete Līdza Deploy plugin that provides a
// pretend "notes" service: each application gets a directory under the
// plugin's state directory and a variable that names it. It shows the hooks
// an application-specific plugin implements and nothing more.
//
// Build it for the server's architecture, pack the program as "plugin" at
// the top of a .tar.gz, then make and sign its manifest with cmd/plugin-sign
// (-hooks describe,check,status,provision,release -env NOTES_DIR).
package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"

	"github.com/agim/lidza-deploy/pluginapi"
)

var appID = regexp.MustCompile(`^[a-z][a-z0-9-]{0,47}$`)

func notesDir(req pluginapi.Request, app string) (string, error) {
	if !appID.MatchString(app) {
		return "", errors.New("invalid application id")
	}
	return filepath.Join(req.StateDir, "notes", app), nil
}

func main() {
	pluginapi.Main(map[string]pluginapi.Handler{
		"describe": func(context.Context, pluginapi.Request) (pluginapi.Answer, error) {
			return pluginapi.Answer{OK: true, Messages: []string{"Keeps a notes directory per application."}}, nil
		},
		// check rehearses provision without changing anything.
		"check": func(_ context.Context, req pluginapi.Request) (pluginapi.Answer, error) {
			const needed = 64 << 20
			check := pluginapi.Check{ID: "disk", Title: "Notes disk", OK: req.Server.StateFreeBytes == 0 || req.Server.StateFreeBytes >= needed, Detail: "The notes directory needs 64 MiB of free space."}
			return pluginapi.Answer{OK: check.OK, Checks: []pluginapi.Check{check}}, nil
		},
		"status": func(_ context.Context, req pluginapi.Request) (pluginapi.Answer, error) {
			entries, _ := os.ReadDir(filepath.Join(req.StateDir, "notes"))
			return pluginapi.Answer{OK: true}.WithOutput(map[string]int{"applications": len(entries)})
		},
		"provision": func(_ context.Context, req pluginapi.Request) (pluginapi.Answer, error) {
			var in pluginapi.AppInput
			if err := req.Decode(&in); err != nil {
				return pluginapi.Answer{}, err
			}
			dir, err := notesDir(req, in.App)
			if err != nil {
				return pluginapi.Answer{}, err
			}
			if err := os.MkdirAll(dir, 0o750); err != nil {
				return pluginapi.Answer{}, err
			}
			// The application's own account owns its notes.
			if in.Account.UID > 0 && os.Geteuid() == 0 {
				if err := os.Chown(dir, in.Account.UID, in.Account.GID); err != nil {
					return pluginapi.Answer{}, err
				}
			}
			return pluginapi.Answer{OK: true}.WithOutput(pluginapi.Provisioned{Env: map[string]string{"NOTES_DIR": dir}, Record: map[string]string{"dir": dir}})
		},
		"release": func(_ context.Context, req pluginapi.Request) (pluginapi.Answer, error) {
			var in pluginapi.AppInput
			if err := req.Decode(&in); err != nil {
				return pluginapi.Answer{}, err
			}
			dir, err := notesDir(req, in.App)
			if err != nil {
				return pluginapi.Answer{}, err
			}
			return pluginapi.Answer{OK: true, Messages: []string{"Removed " + dir + "."}}, os.RemoveAll(dir)
		},
	})
}
