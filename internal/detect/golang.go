package detect

import (
	"path"
	"regexp"
	"slices"
	"strings"

	"golang.org/x/mod/modfile"
)

const lidzaModule = "github.com/agim/lidza"

func (s *scanner) golang(dir string) (*app, error) {
	data, err := s.read(path.Join(dir, "go.mod"))
	if err != nil || data == nil {
		return nil, err
	}
	// Strict parsing reads the toolchain line, which lax parsing skips.
	mod, err := modfile.Parse(path.Join(dir, "go.mod"), data, nil)
	if err != nil {
		mod, err = modfile.ParseLax(path.Join(dir, "go.mod"), data, nil)
	}
	if err != nil || mod.Module == nil {
		return &app{runtime: RuntimeGo, framework: "go", health: "/", reasons: []string{"go.mod could not be parsed; set the process command manually"}, processes: []Process{{Name: "web", Command: "bin/app", Web: true}}}, nil
	}
	a := &app{runtime: RuntimeGo, framework: "go", health: "/"}
	direct := map[string]string{}
	for _, r := range mod.Require {
		if !r.Indirect {
			direct[r.Mod.Path] = r.Mod.Version
		}
	}
	modules := make([]string, 0, len(direct))
	for module := range direct {
		modules = append(modules, module)
	}
	slices.Sort(modules)
	requires := func(prefixes ...string) string {
		for _, prefix := range prefixes {
			for _, module := range modules {
				if module == prefix || strings.HasPrefix(module, prefix+"/") {
					return module
				}
			}
		}
		return ""
	}
	if version, ok := direct[lidzaModule]; ok {
		a.framework, a.version, a.health = "lidza", version, "/healthz"
		a.reason("go.mod requires %s %s: Līdza application", lidzaModule, version)
	} else {
		a.reason("go.mod (module %s): Go application", mod.Module.Mod.Path)
	}

	// The web process runs the built binary of the main package.
	name, pkg, err := s.goMain(dir, mod.Module.Mod.Path, a.framework == "lidza")
	if err != nil {
		return nil, err
	}
	command := "bin/" + name
	a.processes = []Process{{Name: "web", Command: command, Web: true}}
	if a.framework == "lidza" {
		a.reason("Web process: %s built by lidza build", command)
	} else {
		a.reason("Web process: %s built from %s", command, pkg)
	}

	if module := requires("github.com/jackc/pgx", "github.com/lib/pq"); module != "" {
		a.postgres = true
		a.reason("go.mod requires %s: PostgreSQL", module)
	} else if a.framework == "lidza" {
		if ok, err := s.exists(path.Join(dir, "db/migrations")); err != nil {
			return nil, err
		} else if ok {
			a.postgres = true
			a.reason("db/migrations: Līdza database pack (PostgreSQL)")
		}
	}
	if module := requires("github.com/redis/go-redis", "github.com/go-redis/redis", "github.com/gomodule/redigo", "github.com/valkey-io/valkey-go"); module != "" {
		a.redis = true
		a.reason("go.mod requires %s: Redis", module)
	}
	if mod.Go != nil {
		a.pin("go", mod.Go.Version)
	}
	if mod.Toolchain != nil {
		a.pin("go_toolchain", mod.Toolchain.Name)
	}
	if v := a.pins["go"]; v != "" {
		if t := a.pins["go_toolchain"]; t != "" {
			a.reason("go.mod pins Go %s (toolchain %s)", v, t)
		} else {
			a.reason("go.mod pins Go %s", v)
		}
	}
	return a, nil
}

// goMain chooses the main package: the module root, or one command under
// cmd/. It returns the binary name and the package path. Names follow the
// release build: the root package (and every Līdza application) builds to
// bin/<last module path element>, each cmd/<name> to bin/<name>.
func (s *scanner) goMain(dir, module string, lidza bool) (string, string, error) {
	base := path.Base(module)
	if ok, err := s.isFile(path.Join(dir, "main.go")); err != nil || ok || lidza {
		return safeName(base), ".", err
	}
	names, err := s.list(path.Join(dir, "cmd"))
	if err != nil {
		return "", "", err
	}
	var commands []string
	for _, name := range names {
		if strings.HasSuffix(name, "/") && !strings.HasPrefix(name, ".") {
			commands = append(commands, strings.TrimSuffix(name, "/"))
		}
	}
	if len(commands) == 0 {
		return safeName(base), ".", nil
	}
	choice := commands[0]
	for _, preferred := range []string{base, "server", "web", "app", "api"} {
		if slices.Contains(commands, preferred) {
			choice = preferred
			break
		}
	}
	return safeName(choice), "./cmd/" + choice, nil
}

// binaryName matches the release build's binary names; anything else
// builds to bin/app.
var binaryName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

func safeName(name string) string {
	if !binaryName.MatchString(name) {
		return "app"
	}
	return name
}
