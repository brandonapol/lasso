package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/brandonapol/lasso/internal/api"
	"github.com/brandonapol/lasso/internal/config"
	"github.com/brandonapol/lasso/internal/daemon"
)

// openCmd is `lasso open`: make sure the daemon runs, get a one-time
// sign-in code from it, and open the web view with that code. The page
// trades the code for a session cookie at once, so the bearer token never
// goes near a URL and the code in the browser's history is already spent.
func openCmd() *cobra.Command {
	var noBrowser bool
	cmd := &cobra.Command{
		Use:   "open",
		Short: "Open the web view in your browser",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := config.Load()
			if err != nil {
				return err
			}
			state, err := config.StateDir()
			if err != nil {
				return err
			}
			if err := daemon.Ensure(cmd.Context(), daemon.EnsureOptions{
				Port: cfg.API.Port, StateDir: state, Start: daemon.StartBackground(state),
			}); err != nil {
				return err
			}
			dir, err := config.Dir()
			if err != nil {
				return err
			}
			token, err := api.LoadToken(filepath.Join(dir, "token"))
			if err != nil {
				return err
			}
			base := fmt.Sprintf("http://127.0.0.1:%d", cfg.API.Port)
			code, err := signInCode(cmd.Context(), base, token)
			if err != nil {
				return err
			}
			link := base + "/login?code=" + url.QueryEscape(code)
			out := cmd.OutOrStdout()
			if noBrowser {
				fmt.Fprintln(out, link)
				return nil
			}
			if err := openBrowser(link); err != nil {
				fmt.Fprintf(out, "Couldn't open a browser (%v). Open this link within a minute; it works once:\n%s\n", err, link)
				return nil
			}
			fmt.Fprintf(out, "Opened %s/ in your browser.\n", base)
			return nil
		},
	}
	cmd.Flags().BoolVar(&noBrowser, "no-browser", false, "print the one-time sign-in link instead of opening it")
	return cmd
}

// signInCode asks the daemon at base for a one-time sign-in code.
func signInCode(ctx context.Context, base, token string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "POST", base+"/session/code", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	switch {
	case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusMethodNotAllowed:
		return "", fmt.Errorf("the running daemon has no web view yet; stop it and run lasso open again")
	case resp.StatusCode != http.StatusOK:
		return "", fmt.Errorf("the daemon refused a sign-in code: %s", strings.TrimSpace(string(b)))
	}
	var out struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(b, &out); err != nil || out.Code == "" {
		return "", fmt.Errorf("the daemon sent no sign-in code")
	}
	return out.Code, nil
}

// openBrowser opens link in the user's browser (xdg-open, or open on
// macOS).
func openBrowser(link string) error {
	name := "xdg-open"
	if runtime.GOOS == "darwin" {
		name = "open"
	}
	c := exec.Command(name, link)
	if err := c.Start(); err != nil {
		return err
	}
	go func() { _ = c.Wait() }()
	return nil
}
