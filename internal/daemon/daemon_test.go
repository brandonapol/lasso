package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/brandonapol/lasso/internal/api"
	"github.com/brandonapol/lasso/internal/config"
)

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

func testOptions(t *testing.T, port int) Options {
	dir := t.TempDir()
	return Options{
		Port:      port,
		TokenPath: filepath.Join(dir, "config", "token"),
		StateDir:  filepath.Join(dir, "state"),
		Source:    api.NewFakeSource(),
	}
}

// start runs Run in the background and waits until it listens. It is safe
// to call from any goroutine.
func start(t *testing.T, opts Options) error {
	ctx, cancel := context.WithCancel(context.Background())
	ready := make(chan struct{})
	done := make(chan error, 1)
	opts.Ready = func(*net.TCPAddr) { close(ready) }
	go func() { done <- Run(ctx, opts) }()
	select {
	case <-ready:
	case err := <-done:
		cancel()
		return err
	}
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("Run: %v", err)
		}
	})
	return nil
}

func TestRunServesAndWritesPidfile(t *testing.T) {
	port := freePort(t)
	opts := testOptions(t, port)
	if err := start(t, opts); err != nil {
		t.Fatal(err)
	}
	if !Healthy(context.Background(), port) {
		t.Fatal("not healthy")
	}
	b, err := os.ReadFile(PidFile(opts.StateDir))
	if err != nil || strings.TrimSpace(string(b)) == "" {
		t.Fatalf("pidfile: %q %v", b, err)
	}
	fi, err := os.Stat(opts.TokenPath)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("token file: %v %v", fi, err)
	}
}

func TestRunRefusesLooseTokenFile(t *testing.T) {
	for _, mode := range []os.FileMode{0o644, 0o640, 0o604} {
		t.Run(mode.String(), func(t *testing.T) {
			port := freePort(t)
			opts := testOptions(t, port)
			os.MkdirAll(filepath.Dir(opts.TokenPath), 0o700)
			os.WriteFile(opts.TokenPath, []byte("abc\n"), mode)
			os.Chmod(opts.TokenPath, mode)
			err := Run(context.Background(), opts)
			if err == nil || !strings.Contains(err.Error(), "readable by others") {
				t.Fatalf("err = %v", err)
			}
			if Healthy(context.Background(), port) {
				t.Fatal("daemon started anyway")
			}
		})
	}
}

func TestRunFailsClearlyWhenPortTaken(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port
	err = Run(context.Background(), testOptions(t, port))
	if err == nil || !strings.Contains(err.Error(), "already in use") {
		t.Fatalf("err = %v", err)
	}
}

func TestEnsureSharesOneDaemon(t *testing.T) {
	port := freePort(t)
	opts := testOptions(t, port)
	var starts atomic.Int32
	startFn := func() (<-chan error, error) {
		starts.Add(1)
		if err := start(t, opts); err != nil {
			return nil, err
		}
		return make(chan error), nil
	}
	var wg sync.WaitGroup
	errs := make(chan error, 5)
	for range 5 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- Ensure(context.Background(), EnsureOptions{Port: port, StateDir: opts.StateDir, Start: startFn})
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := Ensure(context.Background(), EnsureOptions{Port: port, StateDir: opts.StateDir, Start: startFn}); err != nil {
		t.Fatal(err)
	}
	if n := starts.Load(); n != 1 {
		t.Fatalf("started %d daemons, want 1", n)
	}
}

func TestEnsureReportsEarlyExit(t *testing.T) {
	port := freePort(t)
	exited := make(chan error, 1)
	exited <- errors.New("exit status 1: port taken")
	err := Ensure(context.Background(), EnsureOptions{
		Port: port, StateDir: t.TempDir(), Timeout: 2 * time.Second,
		Start: func() (<-chan error, error) { return exited, nil },
	})
	if err == nil || !strings.Contains(err.Error(), "port taken") {
		t.Fatalf("err = %v", err)
	}
}

// With no Source, Run builds the real one. With no hot repos it makes no gh
// calls and every tab explains how to add them.
func TestRunServesRealSource(t *testing.T) {
	port := freePort(t)
	opts := testOptions(t, port)
	opts.Source = nil
	cfg := config.Default()
	opts.Config = &cfg
	opts.Runner = noGH{t}
	if err := start(t, opts); err != nil {
		t.Fatal(err)
	}
	token, err := os.ReadFile(opts.TokenPath)
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest("GET", fmt.Sprintf("http://127.0.0.1:%d/items?tab=issues", port), nil)
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(token)))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var body struct{ Items []api.Item }
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if len(body.Items) != 1 || !strings.Contains(body.Items[0].Title, "hot_repos") {
		t.Fatalf("items: %+v", body.Items)
	}
	if _, err := os.Stat(DBFile(opts.StateDir)); err != nil {
		t.Fatalf("no database: %v", err)
	}
}

type noGH struct{ t *testing.T }

func (g noGH) Run(_ context.Context, args ...string) ([]byte, error) {
	g.t.Errorf("unexpected gh call %q", args)
	return nil, errors.New("no gh in this test")
}
