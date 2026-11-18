package main

import (
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/backproofhq/runner/internal/agent"
	"github.com/backproofhq/runner/internal/api"
	"github.com/backproofhq/runner/internal/config"
	"github.com/backproofhq/runner/internal/version"
)

func newRegisterCommand(stdout io.Writer, lookup config.LookupFunc) *cobra.Command {
	var token string
	cmd := &cobra.Command{
		Use:   "register",
		Short: "Register this runner with an install token",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := config.Load(lookup)
			if err != nil {
				return fmt.Errorf("load settings: %w", err)
			}
			client, err := api.New(cfg.APIURL, version.Get().Version)
			if err != nil {
				return fmt.Errorf("connect to Backproof: %w", err)
			}

			registration, err := agent.Register(cmd.Context(), client, agent.RegisterOptions{
				InstallToken: config.NewSecret(token),
				Name:         cfg.Name,
				APIURL:       cfg.APIURL,
				FilePath:     config.RunnerEnvFile,
			})
			if err != nil {
				return err
			}

			_, err = fmt.Fprintf(stdout, "Runner %q registered.\nKey fingerprint: %s\nSettings saved to %s\n", registration.Name, registration.Fingerprint, config.RunnerEnvFile)
			if err != nil {
				return fmt.Errorf("print registration: %w", err)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&token, "token", "", "install token from the dashboard's Add runner page")
	if err := cmd.MarkFlagRequired("token"); err != nil {
		panic(err) // The flag is defined on the line above, so this is a programming error.
	}
	return cmd
}
