// Package agent connects the runner to Backproof.
package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"regexp"
	"runtime"
	"unicode"
	"unicode/utf8"

	"github.com/backproofhq/runner/internal/api"
	"github.com/backproofhq/runner/internal/config"
	"github.com/backproofhq/runner/internal/crypto"
)

const (
	maxNameLength = 100
	defaultName   = "backproof-runner"
)

// A token is written to an env file, so a line break in it could add other settings.
var validRunnerToken = regexp.MustCompile(`^[!-~]+$`)

// Registration describes a newly registered runner.
type Registration struct {
	RunnerID    string
	Name        string
	Fingerprint string
}

// RegisterOptions configures a registration.
type RegisterOptions struct {
	InstallToken config.Secret
	Name         string
	APIURL       string
	FilePath     string
}

// Register trades an install token for a runner credential and saves it with a new key pair.
func Register(ctx context.Context, client *api.RunnerClient, options RegisterOptions) (Registration, error) {
	name, err := runnerName(options.Name)
	if err != nil {
		return Registration{}, err
	}

	file, err := config.ReserveRunnerFile(options.FilePath)
	if errors.Is(err, config.ErrRunnerFileExists) {
		return Registration{}, fmt.Errorf("this machine already has a registered runner in %s: remove that file only if you want to register it again", options.FilePath)
	}
	if err != nil {
		return Registration{}, fmt.Errorf("the runner can't write to %s: run it as root or mount that directory, then try again: %w", options.FilePath, err)
	}
	defer file.Discard()

	privateKey, publicKey, err := crypto.GenerateRunnerKey()
	if err != nil {
		return Registration{}, err
	}

	response, err := client.Register(ctx, api.RegisterRequest{
		InstallToken: options.InstallToken.Reveal(),
		Name:         name,
		PublicKey:    publicKey,
		Os:           api.RegisterRequestOs(runtime.GOOS),
		Arch:         api.RegisterRequestArch(runtime.GOARCH),
	})
	if err != nil {
		return Registration{}, fmt.Errorf("register runner: %w", err)
	}
	if !validRunnerToken.MatchString(response.RunnerToken) {
		return Registration{}, errors.New("the Backproof API returned an unusable runner token: create a new install token and try again")
	}

	err = file.Commit(config.RunnerSettings{
		Token:     config.NewSecret(response.RunnerToken),
		RunnerKey: privateKey,
		Name:      name,
		APIURL:    options.APIURL,
	})
	if err != nil {
		return Registration{}, fmt.Errorf("the runner registered but couldn't save its credentials to %s: delete it in the dashboard, create a new install token and try again: %w", options.FilePath, err)
	}

	return Registration{
		RunnerID:    response.RunnerId.String(),
		Name:        name,
		Fingerprint: crypto.KeyFingerprint(publicKey),
	}, nil
}

func runnerName(configured string) (string, error) {
	name := configured
	if name == "" {
		hostname, err := os.Hostname()
		if err != nil || hostname == "" {
			return defaultName, nil
		}
		name = hostname
	}

	invalid := fmt.Errorf("%s is not valid: use at most %d characters without line breaks", config.EnvName, maxNameLength)
	if utf8.RuneCountInString(name) > maxNameLength || !utf8.ValidString(name) {
		return "", invalid
	}
	for _, character := range name {
		if unicode.IsControl(character) {
			return "", invalid
		}
	}
	return name, nil
}
