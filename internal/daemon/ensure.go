package daemon

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
	"time"

	"github.com/brandonapol/lasso/internal/api"
)

// StartFunc starts a daemon and returns a channel that yields once if it
// exits.
type StartFunc func() (exited <-chan error, err error)

// EnsureOptions configures Ensure.
type EnsureOptions struct {
	Port     int
	StateDir string
	Start    StartFunc
	// Timeout bounds the wait for a started daemon to answer (default 5s).
	Timeout time.Duration
}

// Ensure makes sure a daemon answers on the port, starting one if none
// does. Concurrent callers, in this process or others, start at most one:
// they take a lock in StateDir and re-check before starting.
func Ensure(ctx context.Context, opts EnsureOptions) error {
	if opts.Timeout <= 0 {
		opts.Timeout = 5 * time.Second
	}
	if Healthy(ctx, opts.Port) {
		return nil
	}
	unlock, err := lock(filepath.Join(opts.StateDir, "start.lock"))
	if err != nil {
		return err
	}
	defer unlock()
	if Healthy(ctx, opts.Port) {
		return nil
	}
	if pid, ok := livePid(opts.StateDir); ok {
		// A daemon is up but not answering yet, or is stuck: don't start a
		// second one.
		if waitHealthy(ctx, opts.Port, opts.Timeout, nil) == nil {
			return nil
		}
		return fmt.Errorf("lasso daemon (pid %d) is running but not answering on 127.0.0.1:%d", pid, opts.Port)
	}
	exited, err := opts.Start()
	if err != nil {
		return fmt.Errorf("starting lasso daemon: %w", err)
	}
	return waitHealthy(ctx, opts.Port, opts.Timeout, exited)
}

// Healthy reports whether lasso's daemon answers GET /healthz on the port.
func Healthy(ctx context.Context, port int) bool {
	ctx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancel()
	url := "http://" + net.JoinHostPort("127.0.0.1", strconv.Itoa(port)) + "/healthz"
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return false
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64))
	return resp.StatusCode == http.StatusOK && string(b) == api.HealthBody
}

func waitHealthy(ctx context.Context, port int, timeout time.Duration, exited <-chan error) error {
	deadline := time.After(timeout)
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	for {
		if Healthy(ctx, port) {
			return nil
		}
		select {
		case err := <-exited:
			return fmt.Errorf("lasso daemon exited before it was ready: %v", err)
		case <-deadline:
			return fmt.Errorf("lasso daemon didn't answer on 127.0.0.1:%d within %s", port, timeout)
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
		}
	}
}

// livePid reads the pidfile and reports whether that process is alive.
func livePid(stateDir string) (int, bool) {
	b, err := os.ReadFile(PidFile(stateDir))
	if err != nil {
		return 0, false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || pid <= 0 {
		return 0, false
	}
	err = syscall.Kill(pid, 0)
	return pid, err == nil || err == syscall.EPERM
}

// lock takes an exclusive flock on path, blocking until it is free.
func lock(path string) (func(), error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, fmt.Errorf("lock %s: %w", path, err)
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}, nil
}

// StartBackground returns a StartFunc that runs "lasso serve" from this
// executable in its own session, logging to lasso.log in stateDir. If it
// exits early the error names the log's last line.
func StartBackground(stateDir string) StartFunc {
	return func() (<-chan error, error) {
		exe, err := os.Executable()
		if err != nil {
			return nil, err
		}
		if err := os.MkdirAll(stateDir, 0o700); err != nil {
			return nil, err
		}
		logPath := filepath.Join(stateDir, "lasso.log")
		logf, err := os.OpenFile(logPath, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
		if err != nil {
			return nil, err
		}
		defer logf.Close()
		cmd := exec.Command(exe, "serve")
		cmd.Stdout, cmd.Stderr = logf, logf
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		if err := cmd.Start(); err != nil {
			return nil, err
		}
		exited := make(chan error, 1)
		go func() {
			err := cmd.Wait()
			exited <- fmt.Errorf("%v: %s (see %s)", err, lastLine(logPath), logPath)
		}()
		return exited, nil
	}
}

func lastLine(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	return lines[len(lines)-1]
}
