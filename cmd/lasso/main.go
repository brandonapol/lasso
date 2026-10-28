// Command lasso is one quiet window over your repos and bots.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/brandonapol/lasso/internal/api"
	"github.com/brandonapol/lasso/internal/config"
	"github.com/brandonapol/lasso/internal/daemon"
	"github.com/brandonapol/lasso/internal/tui"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	if err := root().Execute(); err != nil {
		os.Exit(1)
	}
}

func root() *cobra.Command {
	cmd := &cobra.Command{
		Use:           "lasso",
		Short:         "One quiet window over your repos and bots",
		SilenceUsage:  true,
		SilenceErrors: false,
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
			return tui.Run(tui.Options{
				Client:   tui.NewClient(fmt.Sprintf("http://127.0.0.1:%d", cfg.API.Port), token, nil),
				Context:  cmd.Context(),
				HotRepos: cfg.HotRepos,
			})
		},
	}
	cmd.AddCommand(
		&cobra.Command{
			Use:   "serve",
			Short: "Run the lasso daemon in the foreground",
			RunE: func(cmd *cobra.Command, _ []string) error {
				return serve(cmd.Context())
			},
		},
		&cobra.Command{
			Use:   "open",
			Short: "Open the web view in your browser",
			RunE: func(cmd *cobra.Command, _ []string) error {
				cfg, err := config.Load()
				if err != nil {
					return err
				}
				fmt.Fprintf(cmd.OutOrStdout(), "http://127.0.0.1:%d/\n", cfg.API.Port)
				return nil
			},
		},
		&cobra.Command{
			Use:   "version",
			Short: "Print the version",
			Run: func(cmd *cobra.Command, _ []string) {
				fmt.Fprintln(cmd.OutOrStdout(), version)
			},
		},
	)
	return cmd
}

// serve runs the daemon until SIGINT or SIGTERM. It serves the real source
// (daemon.Run builds it when Source is nil).
func serve(ctx context.Context) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	dir, err := config.Dir()
	if err != nil {
		return err
	}
	state, err := config.StateDir()
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	return daemon.Run(ctx, daemon.Options{
		Port:      cfg.API.Port,
		TokenPath: filepath.Join(dir, "token"),
		StateDir:  state,
		Config:    &cfg,
	})
}
