// Command fakegh serves the fake GitHub of internal/github/fakegh for local
// demos. Run it from the repository root, so that it finds the fake runner
// in testdata/fakerunner.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/runplane/runplane/internal/github/fakegh"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:9100", "listen address")
	runnerURL := flag.String("runner-url", "", "server URL for fake runners in sandboxes, for example http://host.docker.internal:9100 (default: the URL that runplane uses)")
	runnerDir := flag.String("runner-dir", "testdata/fakerunner", "fake runner bundle, served as each actions/runner release")
	keyPath := flag.String("key", filepath.Join(os.TempDir(), "fakegh-app.pem"), "file to write the app private key to")
	poll := flag.Duration("poll", 10*time.Second, "how long a message poll waits")
	verbose := flag.Bool("v", false, "log each request")
	flag.Parse()

	level := slog.LevelInfo
	if *verbose {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
	if err := run(log, *addr, *runnerURL, *runnerDir, *keyPath, *poll); err != nil {
		log.Error("fakegh", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger, addr, runnerURL, runnerDir, keyPath string, poll time.Duration) error {
	if _, err := os.Stat(filepath.Join(runnerDir, "run.sh")); err != nil {
		log.Warn("no fake runner bundle, so runner downloads get 404", "dir", runnerDir)
		runnerDir = ""
	}
	s := fakegh.New(fakegh.Config{RunnerURL: runnerURL, RunnerDir: runnerDir, PollTimeout: poll, Logger: log})
	if err := os.WriteFile(keyPath, s.PrivateKey(), 0o600); err != nil {
		return fmt.Errorf("write private key: %w", err)
	}

	base := "http://" + localAddr(addr)
	inst := fakegh.DefaultInstallations()[0]
	var repos []string
	for _, r := range inst.Repos {
		repos = append(repos, r.Name)
	}
	fmt.Printf(`fakegh serves %[1]s
  config URL    %[1]s/%[2]s
  API URL       %[1]s/api/v3 (also at the root, as api.github.com)
  app ID        %[3]d, client ID %[4]s
  installation  %[5]d on %[2]s: %[6]s
  private key   %[7]s

Queue a job:
  curl -d '{"repo":"%[8]s","labels":["runplane"],"steps":[{"name":"Run make test","lines":["ok"],"sleep":20}]}' %[1]s/_fake/jobs
See the jobs, runners, and scale sets:
  curl %[1]s/_fake/jobs
  curl %[1]s/_fake/runners
  curl %[1]s/_fake/scale-sets
`, base, inst.Account, fakegh.AppID, fakegh.AppClientID, inst.ID, strings.Join(repos, ", "), keyPath, repos[0])

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return s.Serve(ctx, addr)
}

// localAddr gives the address for a URL on this host: ":9100" and
// "0.0.0.0:9100" become "127.0.0.1:9100".
func localAddr(addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	return net.JoinHostPort(host, port)
}
