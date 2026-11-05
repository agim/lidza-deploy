package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func nativeStaticPublisherFixture(t *testing.T) (*Manager, NativeApplication, string) {
	t.Helper()
	m, app := nativeAstroFixture(t)
	a, err := m.nativeRegistration(m.nativeDiscoveredApplication("marketing", nil, m.nativeObservedRoutes(context.Background())))
	if err != nil {
		t.Fatal(err)
	}
	m.cfg.Native.StaticDeployments = map[string]NativeStaticPolicy{a.ID: {Enabled: true, ExternalDeploymentsDisabled: true, Repository: a.Repository, Domain: a.Domain, OutputDirectory: "dist"}}
	old := filepath.Join(app, "current/public/_astro")
	os.MkdirAll(old, 0755)
	os.WriteFile(filepath.Join(old, "old.hash.js"), []byte("old hashed asset"), 0644)
	m.nativeCheckout = func(ctx context.Context, _ App, candidate, _ string) error {
		if err := os.MkdirAll(candidate, 0755); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(candidate, "package.json"), []byte(`{"dependencies":{"astro":"7.4.4"},"scripts":{"build":"astro build"}}`), 0644); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(candidate, "package-lock.json"), []byte(`{}`), 0644); err != nil {
			return err
		}
		for _, args := range [][]string{{"init", "-q"}, {"add", "."}, {"-c", "user.name=Fixture", "-c", "user.email=fixture@example.com", "commit", "-qm", "Static release"}} {
			if _, err := command(ctx, candidate, nil, "git", args...); err != nil {
				return err
			}
		}
		return nil
	}
	m.nativeBuildCommand = func(_ context.Context, _ NativeApplication, candidate string, args []string) (string, error) {
		if strings.Join(args, " ") == "npm run build" {
			os.MkdirAll(filepath.Join(candidate, "dist/_astro"), 0755)
			os.WriteFile(filepath.Join(candidate, "dist/index.html"), []byte("<h1>New release</h1>"), 0644)
			os.WriteFile(filepath.Join(candidate, "dist/_astro/new.hash.js"), []byte("new hashed asset"), 0644)
		}
		return "", nil
	}
	return m, a, app
}

func TestNativeStaticPublicationKeepsOldReleaseAssetsAndRollsBackWithoutServiceCommands(t *testing.T) {
	m, a, app := nativeStaticPublisherFixture(t)
	commands := 0
	m.nativeCommand = func(context.Context, []string) (string, error) { commands++; return "", nil }
	old, _ := filepath.EvalSymlinks(filepath.Join(app, "current"))
	environment, _ := os.ReadFile(filepath.Join(app, ".asdf-vars"))
	release, err := m.prepareNativeStatic(context.Background(), a, newID(), "")
	if err != nil {
		t.Fatal(err)
	}
	if current, _ := filepath.EvalSymlinks(filepath.Join(app, "current")); current != old {
		t.Fatal("build changed the serving release")
	}
	if err = m.publishNativeStatic(a.ID, release); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"index.html", "_astro/old.hash.js", "_astro/new.hash.js"} {
		if _, err := os.Stat(filepath.Join(app, "current/public", path)); err != nil {
			t.Fatal("published release dropped an existing asset", path)
		}
	}
	if err = m.rollbackNativeStatic(a.ID); err != nil {
		t.Fatal(err)
	}
	if current, _ := filepath.EvalSymlinks(filepath.Join(app, "current")); current != old {
		t.Fatal("rollback did not select the retained release")
	}
	after, _ := os.ReadFile(filepath.Join(app, ".asdf-vars"))
	if string(after) != string(environment) || commands != 0 {
		t.Fatal("publication changed environment or a service")
	}
	if len(m.data.NativeTransitions) != 0 {
		t.Fatal("successful publication left a pending transition")
	}
}

func TestNativeStaticBuildFailureAndRuntimeDriftRetainCurrentRelease(t *testing.T) {
	for _, scenario := range []string{"build-failure", "runtime-change", "asset-collision", "unsafe-output"} {
		t.Run(scenario, func(t *testing.T) {
			m, a, app := nativeStaticPublisherFixture(t)
			old, _ := os.Readlink(filepath.Join(app, "current"))
			base := m.nativeBuildCommand
			switch scenario {
			case "build-failure":
				m.nativeBuildCommand = func(context.Context, NativeApplication, string, []string) (string, error) {
					return "", errors.New("build failed")
				}
			case "runtime-change":
				os.WriteFile(filepath.Join(app, "current/.node-version"), []byte("22.23.4"), 0644)
				checkout := m.nativeCheckout
				m.nativeCheckout = func(ctx context.Context, a App, candidate, token string) error {
					if err := checkout(ctx, a, candidate, token); err != nil {
						return err
					}
					return os.WriteFile(filepath.Join(candidate, ".node-version"), []byte("24.5.0"), 0644)
				}
			case "asset-collision":
				m.nativeBuildCommand = func(ctx context.Context, a NativeApplication, path string, args []string) (string, error) {
					out, err := base(ctx, a, path, args)
					if strings.Join(args, " ") == "npm run build" {
						os.WriteFile(filepath.Join(path, "dist/_astro/old.hash.js"), []byte("different old asset"), 0644)
					}
					return out, err
				}
			case "unsafe-output":
				m.nativeBuildCommand = func(ctx context.Context, a NativeApplication, path string, args []string) (string, error) {
					out, err := base(ctx, a, path, args)
					if strings.Join(args, " ") == "npm run build" {
						os.Symlink("/etc/passwd", filepath.Join(path, "dist/private"))
					}
					return out, err
				}
			}
			if _, err := m.prepareNativeStatic(context.Background(), a, newID(), ""); err == nil {
				t.Fatal("unsafe or failed candidate accepted")
			}
			if current, _ := os.Readlink(filepath.Join(app, "current")); current != old {
				t.Fatal("failure altered the serving release")
			}
		})
	}
}

func TestNativeStaticDeployRequiresExplicitOwnershipPolicy(t *testing.T) {
	m, a, _ := nativeStaticPublisherFixture(t)
	m.cfg.Native.StaticDeployments = nil
	if _, err := m.enqueueNativeStatic(a.ID, DeployRequest{}); err == nil {
		t.Fatal("native deployment enabled by discovery")
	}
	if len(m.data.Deployments) != 0 {
		t.Fatal("unapproved deployment was queued")
	}
}

func TestNativeStaticQueueDeduplicatesDeliveriesAndRejectsUnreviewedSource(t *testing.T) {
	m, a, _ := nativeStaticPublisherFixture(t)
	first, err := m.enqueueNativeStatic(a.ID, DeployRequest{Key: "delivery-one"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := m.enqueueNativeStatic(a.ID, DeployRequest{Key: "delivery-one"})
	m.mu.Lock()
	count := len(m.data.Deployments)
	m.mu.Unlock()
	if err != nil || first.ID != second.ID || count != 1 {
		t.Fatal("a redelivered webhook queued a second native publication", err)
	}
	for _, scenario := range []string{"manifest", "runtime"} {
		t.Run(scenario, func(t *testing.T) {
			dir := t.TempDir()
			os.WriteFile(filepath.Join(dir, "package.json"), []byte(`{"dependencies":{"astro":"7"},"scripts":{"build":"astro build"}}`), 0600)
			os.WriteFile(filepath.Join(dir, "package-lock.json"), []byte(`{}`), 0600)
			switch scenario {
			case "manifest":
				os.WriteFile(filepath.Join(dir, "package.json"), []byte(`{}`), 0600)
			case "runtime":
				os.WriteFile(filepath.Join(dir, "Gemfile"), nil, 0600)
			}
			if validateNativeAstroSource(dir) == nil {
				t.Fatal("unreviewed source was accepted")
			}
		})
	}
}

func TestNativePublicationRecoveryOnlyReconcilesMetadata(t *testing.T) {
	for _, scenario := range []string{"before", "after", "external"} {
		t.Run(scenario, func(t *testing.T) {
			m, a, app := nativeStaticPublisherFixture(t)
			old, _ := filepath.EvalSymlinks(filepath.Join(app, "current"))
			release, err := m.prepareNativeStatic(context.Background(), a, newID(), "")
			if err != nil {
				t.Fatal(err)
			}
			before := NativeReleaseHistory{Current: NativeReleaseRef{ID: "one", Path: old}}
			after := NativeReleaseHistory{Current: release, Previous: before.Current}
			m.data.NativeTransitions = map[string]NativeReleaseTransition{a.ID: {Before: before, After: after}}
			if scenario != "before" {
				target := release.Path
				if scenario == "external" {
					target = filepath.Join(app, "releases/external")
					os.MkdirAll(target, 0755)
				}
				if err = replaceNativeCurrent(filepath.Join(app, "current"), old, target); err != nil {
					t.Fatal(err)
				}
			}
			actual, _ := os.Readlink(filepath.Join(app, "current"))
			if err = m.recoverNativePublications(); err != nil {
				t.Fatal(err)
			}
			if current, _ := os.Readlink(filepath.Join(app, "current")); current != actual {
				t.Fatal("recovery changed an application release")
			}
			if scenario == "external" {
				if len(m.data.NativeTransitions) != 1 || m.data.NativeTransitions[a.ID].Error == "" {
					t.Fatal("external ownership change was ignored")
				}
			} else if len(m.data.NativeTransitions) != 0 {
				t.Fatal("deterministic metadata recovery did not complete")
			}
		})
	}
}

func TestNativeStaticFirstPublicationRejectsAnExternalDeploymentDuringBuild(t *testing.T) {
	m, a, app := nativeStaticPublisherFixture(t)
	release, err := m.prepareNativeStatic(context.Background(), a, newID(), "")
	if err != nil {
		t.Fatal(err)
	}
	old, _ := filepath.EvalSymlinks(filepath.Join(app, "current"))
	external := filepath.Join(app, "releases", "external")
	os.MkdirAll(filepath.Join(external, "public"), 0755)
	os.WriteFile(filepath.Join(external, "package.json"), []byte(`{"dependencies":{"astro":"7"}}`), 0600)
	os.WriteFile(filepath.Join(external, "public/index.html"), []byte("external release"), 0600)
	if err = replaceNativeCurrent(filepath.Join(app, "current"), old, external); err != nil {
		t.Fatal(err)
	}
	if err = m.publishNativeStatic(a.ID, release); err == nil {
		t.Fatal("first takeover overwrote an intervening external deployment")
	}
	actual, _ := filepath.EvalSymlinks(filepath.Join(app, "current"))
	if actual != external {
		t.Fatal("refused publication changed the current release")
	}
}
