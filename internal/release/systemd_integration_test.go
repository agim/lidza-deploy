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
	"strings"
	"sync"
	"testing"
	"time"
)

// TestUserSystemdSocketActivatedPumaDeployAndRollback deploys a changed
// commit and rolls it back on real systemd user units shaped like a
// conventional native Rails host: a socket unit owning the port, a Puma
// service started from current with inherited descriptors, and a worker.
// Requests run continuously throughout; none may fail, and a request in
// flight during the restart must finish on the old process.
//
// It needs a running systemd user manager, Ruby with puma and rack gems, and
// opts in with LIDZA_RELEASE_SYSTEMD_IT=1. Only units named
// lidza-release-it-* are created, and they are removed afterwards.
func TestUserSystemdSocketActivatedPumaDeployAndRollback(t *testing.T) {
	if os.Getenv("LIDZA_RELEASE_SYSTEMD_IT") != "1" {
		t.Skip("set LIDZA_RELEASE_SYSTEMD_IT=1 to run against the systemd user manager")
	}
	runtimeDir := os.Getenv("XDG_RUNTIME_DIR")
	home, _ := os.UserHomeDir()
	if runtimeDir == "" || home == "" {
		t.Fatal("needs XDG_RUNTIME_DIR and HOME")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	path := os.Getenv("PATH")
	baseEnv := []string{"PATH=" + path, "HOME=" + home, "XDG_RUNTIME_DIR=" + runtimeDir, "LANG=C.UTF-8"}

	// Source repository: a Rack app served by Puma, plus a worker.
	url, commit := gitFixture(t)
	work := strings.TrimPrefix(url, "file://")
	commit("Gemfile", "source \"https://rubygems.org\"\ngem \"puma\"\ngem \"rack\"\n")
	commit("config.ru", `run lambda { |env|
  sleep 3 if env["PATH_INFO"] == "/slow"
  [200, {"content-type" => "text/plain"}, [File.read(File.join(__dir__, "VERSION"))]]
}
`)
	commit("worker.rb", "trap(\"TERM\") { exit 0 }\nloop { File.write(ARGV[0], File.read(File.join(__dir__, \"VERSION\"))); sleep 0.2 }\n")
	lock := exec.CommandContext(ctx, "bundle", "lock", "--local")
	lock.Dir, lock.Env = work, append(baseEnv, "BUNDLE_GEMFILE="+filepath.Join(work, "Gemfile"))
	if out, err := lock.CombinedOutput(); err != nil {
		t.Fatalf("bundle lock: %v %s", err, out)
	}
	commit("VERSION", "one")

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()

	root := filepath.Join(t.TempDir(), "app")
	os.Mkdir(root, 0o755)
	name := fmt.Sprintf("lidza-release-it-%d", time.Now().UnixNano()%1_000_000_000)
	server, socket, worker := name+"-server.service", name+"-server.socket", name+"-worker.service"
	unitDir := filepath.Join(home, ".config/systemd/user")
	os.MkdirAll(unitDir, 0o755)
	envFile := filepath.Join(root, ".env.units")
	os.WriteFile(envFile, []byte(fmt.Sprintf("PORT=%d\nRACK_ENV=production\n", port)), 0o600)
	heartbeat := filepath.Join(root, "worker.version")
	service := func(description, start string, extra string) string {
		return fmt.Sprintf("[Unit]\nDescription=%s\n%s\n[Service]\nType=simple\nWorkingDirectory=%s\nEnvironment=PATH=%s\nEnvironment=PWD=%s\nEnvironmentFile=-%s\nExecStart=bash -c '%s'\nRestart=always\n",
			description, extra, filepath.Join(root, "current"), path, filepath.Join(root, "current"), envFile, start)
	}
	units := map[string]string{
		socket: fmt.Sprintf("[Unit]\nDescription=%s sockets\n[Socket]\nListenStream=127.0.0.1:%d\nReusePort=true\nBacklog=1024\n", name, port),
		server: service(name+" server", "bundle exec --keep-file-descriptors puma -b tcp://127.0.0.1:$PORT", "Requires="+socket),
		worker: service(name+" worker", "bundle exec ruby worker.rb "+heartbeat, ""),
	}
	systemctl := func(args ...string) {
		cmd := exec.Command("systemctl", append([]string{"--user"}, args...)...)
		cmd.Env = baseEnv
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Logf("systemctl %v: %v %s", args, err, out)
		}
	}
	for unit, body := range units {
		if err := os.WriteFile(filepath.Join(unitDir, unit), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		systemctl("stop", server, worker, socket)
		systemctl("reset-failed", server, worker, socket)
		for unit := range units {
			os.Remove(filepath.Join(unitDir, unit))
		}
		systemctl("daemon-reload")
	})
	systemctl("daemon-reload")

	executor := LocalExecutor{}
	plan := Plan{
		App: name, Layout: Layout{Root: root}, Source: Source{URL: url}, Ref: "main",
		Profile: Rails(RailsOptions{Shared: &Shared{Dirs: []string{"log", "tmp"}}}),
		Env:     baseEnv, Exec: executor, SkipBackup: true,
		Services: UserSystemd{Exec: executor, Env: baseEnv},
		Units:    []string{server, worker}, Settle: 30 * time.Second, Keep: 3,
		Log: testLog{t},
	}
	client := &http.Client{Timeout: 15 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}
	get := func(pathname string) (string, error) {
		resp, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d%s", port, pathname))
		if err != nil {
			return "", err
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		if resp.StatusCode != 200 {
			return "", fmt.Errorf("HTTP %d", resp.StatusCode)
		}
		return string(body), err
	}

	// First release; the server is socket activated by the first request.
	first, err := Deploy(ctx, plan)
	if err != nil {
		t.Fatal("initial deploy", err)
	}
	systemctl("start", socket, worker)
	if body, err := get("/"); err != nil || body != "one" {
		t.Fatal("first release not served", body, err)
	}
	plan.Health = func(context.Context) error { _, err := get("/"); return err }

	// load runs requests continuously until stop is closed.
	load := func(stop <-chan struct{}) (seen map[string]int, failures []string) {
		var mu sync.Mutex
		seen = map[string]int{}
		var wg sync.WaitGroup
		for i := 0; i < 4; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for {
					select {
					case <-stop:
						return
					default:
					}
					body, err := get("/")
					mu.Lock()
					if err != nil {
						failures = append(failures, err.Error())
					} else {
						seen[body]++
					}
					mu.Unlock()
					time.Sleep(20 * time.Millisecond)
				}
			}()
		}
		<-stop
		wg.Wait()
		return seen, failures
	}
	phase := func(label string, operate func() error, before, after string) {
		t.Helper()
		stop := make(chan struct{})
		type outcome struct {
			seen     map[string]int
			failures []string
		}
		done := make(chan outcome)
		go func() { s, f := load(stop); done <- outcome{s, f} }()
		var slowBody string
		var slowErr error
		slow := make(chan struct{})
		go func() { slowBody, slowErr = get("/slow"); close(slow) }()
		time.Sleep(500 * time.Millisecond) // the slow request is in flight on the old process
		if err := operate(); err != nil {
			close(stop)
			<-done
			t.Fatal(label, err)
		}
		time.Sleep(time.Second)
		close(stop)
		result := <-done
		<-slow
		if len(result.failures) > 0 {
			t.Fatalf("%s: %d failed requests, first: %s", label, len(result.failures), result.failures[0])
		}
		if slowErr != nil || slowBody != before {
			t.Fatalf("%s: in-flight request got %q, %v (want %q from the old process)", label, slowBody, slowErr, before)
		}
		if result.seen[after] == 0 {
			t.Fatalf("%s: never served %q: %v", label, after, result.seen)
		}
		if body, _ := get("/"); body != after {
			t.Fatalf("%s: serving %q after operation", label, body)
		}
		deadline := time.Now().Add(15 * time.Second)
		for {
			raw, _ := os.ReadFile(heartbeat)
			if string(raw) == after {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s: worker still runs %q", label, raw)
			}
			time.Sleep(200 * time.Millisecond)
		}
		t.Logf("%s: %d requests, 0 failures, served %v; in-flight request finished on %q", label,
			result.seen[before]+result.seen[after], result.seen, slowBody)
	}

	commit("VERSION", "two")
	var second Result
	phase("deploy", func() error { second, err = Deploy(ctx, plan); return err }, "one", "two")
	if second.Previous != first.Release || len(second.Restarted) != 2 {
		t.Fatalf("deploy result %+v", second)
	}
	var back Result
	phase("rollback", func() error { back, err = Rollback(ctx, plan, ""); return err }, "two", "one")
	if back.Release != first.Release {
		t.Fatalf("rollback result %+v", back)
	}
	if _, err := os.Stat(filepath.Join(root, "shared/bundle")); err != nil {
		t.Fatal("gems were not installed into shared/bundle")
	}
}

type testLog struct{ t *testing.T }

func (l testLog) Write(p []byte) (int, error) {
	l.t.Log(strings.TrimRight(string(p), "\n"))
	return len(p), nil
}
