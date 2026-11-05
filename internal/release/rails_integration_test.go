package release

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Real-application proofs. They are opted into with LIDZA_RELEASE_RAILS_IT=1
// and take their inputs from the environment:
//
//	LIDZA_RELEASE_RAILS_SOURCE        git checkout of a Rails application
//	LIDZA_RELEASE_RAILS_DATABASE_URL  disposable PostgreSQL database for it
//	LIDZA_RELEASE_ASTRO_SOURCE        git checkout of a static (Astro) site
//	LIDZA_RELEASE_TOOLCHAIN_PATH      PATH holding the pinned ruby/bundle/node/npm
//
// They need network access for gem and package installation.

func realAppEnv(t *testing.T, home string, extra ...string) []string {
	t.Helper()
	path := os.Getenv("LIDZA_RELEASE_TOOLCHAIN_PATH")
	if path == "" {
		path = os.Getenv("PATH")
	}
	return append([]string{"PATH=" + path, "HOME=" + home, "LANG=C.UTF-8", "TERM=dumb"}, extra...)
}

// cloneWithProbeCommit clones source and adds one comment-only commit so the
// test has an original and a changed commit to move between.
func cloneWithProbeCommit(t *testing.T, source, probeFile string) (url, original, changed string) {
	t.Helper()
	clone := filepath.Join(t.TempDir(), "source")
	git := func(args ...string) string {
		cmd := exec.Command("git", args...)
		cmd.Dir = clone
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=release-it", "GIT_AUTHOR_EMAIL=it@example.com",
			"GIT_COMMITTER_NAME=release-it", "GIT_COMMITTER_EMAIL=it@example.com")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	if out, err := exec.Command("git", "clone", "--quiet", "file://"+source, clone).CombinedOutput(); err != nil {
		t.Fatalf("clone %s: %v %s", source, err, out)
	}
	original = git("rev-parse", "HEAD")
	path := filepath.Join(clone, probeFile)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(raw, "\n# release engine probe\n"...), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", probeFile)
	git("commit", "--quiet", "-m", "release engine probe")
	changed = git("rev-parse", "HEAD")
	return "file://" + clone, original, changed
}

func freePort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	return listener.Addr().(*net.TCPAddr).Port
}

func TestRealRailsApplicationDeployMigrateBootAndRollback(t *testing.T) {
	if os.Getenv("LIDZA_RELEASE_RAILS_IT") != "1" {
		t.Skip("set LIDZA_RELEASE_RAILS_IT=1 with LIDZA_RELEASE_RAILS_SOURCE and LIDZA_RELEASE_RAILS_DATABASE_URL")
	}
	source, databaseURL := os.Getenv("LIDZA_RELEASE_RAILS_SOURCE"), os.Getenv("LIDZA_RELEASE_RAILS_DATABASE_URL")
	if source == "" || databaseURL == "" {
		t.Skip("LIDZA_RELEASE_RAILS_SOURCE and LIDZA_RELEASE_RAILS_DATABASE_URL are required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Minute)
	defer cancel()
	home, _ := os.UserHomeDir()
	url, original, changed := cloneWithProbeCommit(t, source, "config/initializers/inflections.rb")
	root := filepath.Join(t.TempDir(), "app")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	env := realAppEnv(t, home,
		"DATABASE_URL="+databaseURL, "RAILS_ENV=production", "RACK_ENV=production",
		"SECRET_KEY_BASE="+strings.Repeat("ab", 32), "RAILS_LOG_TO_STDOUT=1", "RAILS_MAX_THREADS=3",
		"BUNDLE_WITHOUT=development:test")
	plan := Plan{
		App: "rails-it", Layout: Layout{Root: root}, Source: Source{URL: url}, Ref: original,
		Profile: Rails(RailsOptions{}), Env: env, Exec: LocalExecutor{}, SkipBackup: true, Keep: 3, Log: testLog{t},
	}
	l := plan.Layout
	rails := func(dir string, extraEnv []string, args ...string) string {
		t.Helper()
		cmd := exec.CommandContext(ctx, "bundle", append([]string{"exec", "rails"}, args...)...)
		cmd.Dir, cmd.Env = dir, append(plan.stepEnv(dir, Step{}), extraEnv...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("rails %v: %v\n%s", args, err, out)
		}
		lines := strings.Split(strings.TrimSpace(string(out)), "\n")
		return strings.TrimSpace(lines[len(lines)-1])
	}

	started := time.Now()
	first, err := Deploy(ctx, plan)
	if err != nil {
		t.Fatal("first deploy:", err)
	}
	t.Logf("first deploy took %s", time.Since(started).Round(time.Second))
	if first.Commit != original || !first.Migrated || first.Previous != "" {
		t.Fatalf("first result %+v", first)
	}
	release := l.Release(first.Release)
	if rev, _ := l.Revision(first.Release); rev != original {
		t.Fatal("REVISION", rev)
	}
	for _, link := range []string{"log", "storage", "tmp", "public/system", "public/uploads"} {
		target, err := os.Readlink(filepath.Join(release, link))
		if err != nil || !strings.HasSuffix(target, "/shared/"+link) {
			t.Fatalf("shared link %s -> %q (%v)", link, target, err)
		}
	}
	bundleConfig := readFile(t, filepath.Join(release, ".bundle/config"))
	if !strings.Contains(bundleConfig, filepath.Join(l.Shared(), "bundle")) || !strings.Contains(bundleConfig, "BUNDLE_DEPLOYMENT: \"true\"") {
		t.Fatalf(".bundle/config:\n%s", bundleConfig)
	}
	if gems, _ := filepath.Glob(filepath.Join(l.Shared(), "bundle/ruby/*/gems/rails-*")); len(gems) == 0 {
		t.Fatal("gems were not installed into shared/bundle")
	}
	if _, err := os.Stat(filepath.Join(release, "node_modules/.bin/vite")); err != nil {
		t.Fatal("npm ci did not install vite", err)
	}
	manifests, _ := filepath.Glob(filepath.Join(release, "public/vite/.vite/manifest*.json"))
	if len(manifests) == 0 {
		manifests, _ = filepath.Glob(filepath.Join(release, "public/vite/manifest*.json"))
	}
	if len(manifests) == 0 {
		t.Fatal("assets:precompile produced no Vite manifest under public/vite")
	}
	assets, _ := filepath.Glob(filepath.Join(release, "public/vite/assets/*"))
	t.Logf("vite manifest %s, %d built assets", filepath.Base(manifests[0]), len(assets))

	// Every database of this application shares one DATABASE_URL. db:prepare
	// loads the primary schema into a fresh database; the cache and cable
	// schemas are only loaded here for the disposable database so the
	// application boots fully. Production databases already hold them.
	current := l.Current()
	// The cache and cable schemas target the same database as primary, so
	// they are loaded through the primary connection of the disposable test
	// database (the db:schema:load:<name> tasks need a test-environment config).
	rails(current, nil, "runner", `ActiveRecord::Schema.verbose = false; %w[cache cable].each { |name| load Rails.root.join("db/#{name}_schema.rb") }`)
	migrationFiles, _ := filepath.Glob(filepath.Join(release, "db/migrate/*.rb"))
	// A fresh database receives the schema, which records the schema version
	// and every migration file's version as applied.
	applied := rails(current, nil, "runner", `puts ActiveRecord::Base.connection.select_values("select version from schema_migrations").sort.join(",")`)
	for _, file := range migrationFiles {
		version, _, _ := strings.Cut(filepath.Base(file), "_")
		if !strings.Contains(applied, version) {
			t.Fatalf("migration %s not applied; schema_migrations: %s", version, applied)
		}
	}
	if len(migrationFiles) == 0 || strings.Count(applied, ",")+1 < len(migrationFiles) {
		t.Fatalf("schema_migrations %q, want at least %d versions", applied, len(migrationFiles))
	}
	tables := rails(current, nil, "runner", `puts ActiveRecord::Base.connection.tables.sort.join(",")`)
	for _, table := range []string{"solid_queue_jobs", "solid_cache_entries", "solid_cable_messages"} {
		if !strings.Contains(tables, table) {
			t.Fatalf("table %s missing from %s", table, tables)
		}
	}
	t.Logf("%d migration files, %d versions applied; tables: %s", len(migrationFiles), strings.Count(applied, ",")+1, tables)

	bootRelease := func(label string, wantRevision string) {
		t.Helper()
		port := freePort(t)
		puma := exec.CommandContext(ctx, "bundle", "exec", "puma", "-b", fmt.Sprintf("tcp://127.0.0.1:%d", port), "-e", "production")
		puma.Dir = current
		puma.Env = append(plan.stepEnv(current, Step{}), "PORT="+strconv.Itoa(port))
		puma.Stdout, puma.Stderr = testLog{t}, testLog{t}
		puma.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		if err := puma.Start(); err != nil {
			t.Fatal(err)
		}
		defer func() {
			syscall.Kill(-puma.Process.Pid, syscall.SIGTERM)
			puma.Wait()
		}()
		client := &http.Client{Timeout: 5 * time.Second}
		deadline := time.Now().Add(90 * time.Second)
		for {
			req, _ := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d/up", port), nil)
			req.Header.Set("X-Forwarded-Proto", "https") // the application forces SSL behind its reverse proxy
			resp, err := client.Do(req)
			if err == nil {
				body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
				resp.Body.Close()
				if resp.StatusCode == http.StatusOK {
					t.Logf("%s: GET /up returned 200 from %s (%d bytes)", label, wantRevision[:12], len(body))
					return
				}
				if time.Now().After(deadline) {
					t.Fatalf("%s: GET /up returned %d: %s", label, resp.StatusCode, body)
				}
			} else if time.Now().After(deadline) {
				t.Fatalf("%s: application did not boot: %v", label, err)
			}
			time.Sleep(500 * time.Millisecond)
		}
	}
	bootRelease("first release", original)

	plan.Ref = changed
	started = time.Now()
	second, err := Deploy(ctx, plan)
	if err != nil {
		t.Fatal("second deploy:", err)
	}
	t.Logf("second deploy took %s", time.Since(started).Round(time.Second))
	if second.Commit != changed || second.Previous != first.Release {
		t.Fatalf("second result %+v", second)
	}
	if got := readFile(t, filepath.Join(l.Current(), "config/initializers/inflections.rb")); !strings.Contains(got, "release engine probe") {
		t.Fatal("current does not serve the changed commit")
	}
	bootRelease("changed release", changed)

	back, err := Rollback(ctx, plan, "")
	if err != nil || back.Release != first.Release || back.Commit != original {
		t.Fatal("rollback:", err, back)
	}
	if got := readFile(t, filepath.Join(l.Current(), "config/initializers/inflections.rb")); strings.Contains(got, "release engine probe") {
		t.Fatal("rollback did not restore the original commit")
	}
	bootRelease("rolled back release", original)
	ids, _ := l.List()
	t.Logf("releases retained: %v", ids)
}

func TestRealStaticSiteDeploy(t *testing.T) {
	if os.Getenv("LIDZA_RELEASE_RAILS_IT") != "1" || os.Getenv("LIDZA_RELEASE_ASTRO_SOURCE") == "" {
		t.Skip("set LIDZA_RELEASE_RAILS_IT=1 and LIDZA_RELEASE_ASTRO_SOURCE")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	home, _ := os.UserHomeDir()
	url, original, changed := cloneWithProbeCommit(t, os.Getenv("LIDZA_RELEASE_ASTRO_SOURCE"), "README.md")
	root := filepath.Join(t.TempDir(), "site")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "detect")
	if out, err := exec.Command("git", "clone", "--quiet", "--depth=1", url, dir).CombinedOutput(); err != nil {
		t.Fatal(string(out), err)
	}
	profile, err := Detect(dir)
	if err != nil || profile.Name != "static" || profile.Output != "dist" {
		t.Fatal("detection", profile.Name, err)
	}
	plan := Plan{App: "static-it", Layout: Layout{Root: root}, Source: Source{URL: url}, Ref: original,
		Profile: profile, Env: realAppEnv(t, home), Exec: LocalExecutor{}, Keep: 2, Log: testLog{t}}
	started := time.Now()
	first, err := Deploy(ctx, plan)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("static deploy took %s", time.Since(started).Round(time.Second))
	index := filepath.Join(plan.Layout.Current(), "dist/index.html")
	if info, err := os.Stat(index); err != nil || info.Size() == 0 {
		t.Fatal("dist/index.html missing", err)
	}
	pages := 0
	filepath.WalkDir(filepath.Join(plan.Layout.Current(), "dist"), func(path string, entry os.DirEntry, err error) error {
		if err == nil && !entry.IsDir() && entry.Name() == "index.html" {
			pages++
		}
		return nil
	})
	t.Logf("built %d pages from %s", pages, first.Commit[:12])
	plan.Ref = changed
	if _, err := Deploy(ctx, plan); err != nil {
		t.Fatal(err)
	}
	if back, err := Rollback(ctx, plan, ""); err != nil || back.Release != first.Release {
		t.Fatal("rollback", err)
	}
	if _, err := os.Stat(index); err != nil {
		t.Fatal("rolled back dist missing", err)
	}
}
