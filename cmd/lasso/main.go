// Command lasso is one quiet window over your repos and bots.
package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/brandonapol/lasso/internal/config"
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
			if _, err := config.Load(); err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), "lasso: the TUI isn't built yet (#13)")
			return nil
		},
	}
	cmd.AddCommand(
		&cobra.Command{
			Use:   "serve",
			Short: "Run the lasso daemon in the foreground",
			RunE: func(cmd *cobra.Command, _ []string) error {
				if _, err := config.Load(); err != nil {
					return err
				}
				fmt.Fprintln(cmd.OutOrStdout(), "lasso serve: the daemon isn't built yet (#2)")
				return nil
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
