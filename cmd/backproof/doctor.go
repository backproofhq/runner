package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/mod/semver"

	"github.com/backproofhq/runner/internal/api"
	"github.com/backproofhq/runner/internal/config"
	"github.com/backproofhq/runner/internal/crypto"
	"github.com/backproofhq/runner/internal/docker"
	"github.com/backproofhq/runner/internal/version"
)

const (
	doctorDockerTimeout = 10 * time.Second
	updateCommand       = "curl -fsSL https://get.backproof.dev | sh"
)

type checkStatus string

const (
	checkOK   checkStatus = "ok"
	checkWarn checkStatus = "warn"
	checkFail checkStatus = "fail"
)

type doctorCheck struct {
	status checkStatus
	name   string
	detail string
}

func newDoctorCommand(stdout io.Writer, lookup config.LookupFunc) *cobra.Command {
	return &cobra.Command{
		Use:   "doctor",
		Short: "Check this runner's setup",
		Long:  "Checks the settings, Docker, the connection to Backproof, the runner's key and version, and which sources this runner has keys for. It never prints a key and never claims a job. It exits with 2 when a check fails.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			checks := runDoctor(cmd.Context(), lookup)
			failed, warned := 0, 0
			var report strings.Builder
			for _, check := range checks {
				switch check.status {
				case checkFail:
					failed++
				case checkWarn:
					warned++
				}
				fmt.Fprintf(&report, "%-4s  %s: %s\n", check.status, check.name, check.detail)
			}
			switch {
			case failed > 0:
				fmt.Fprintf(&report, "%d of %d checks failed.\n", failed, len(checks))
			case warned > 0:
				fmt.Fprintf(&report, "No checks failed, %d need attention.\n", warned)
			default:
				report.WriteString("No problems found.\n")
			}
			if _, err := io.WriteString(stdout, report.String()); err != nil {
				return fmt.Errorf("print checks: %w", err)
			}
			if failed > 0 {
				return exitCodeError(exitRunnerError)
			}
			return nil
		},
	}
}

func runDoctor(ctx context.Context, lookup config.LookupFunc) []doctorCheck {
	settings, cfg, settingsCheck := doctorSettings(lookup)
	checks := []doctorCheck{settingsCheck, doctorDocker(ctx)}
	if settingsCheck.status == checkFail {
		return checks
	}

	status, apiCheck := doctorAPI(ctx, cfg)
	checks = append(checks, apiCheck)
	if apiCheck.status == checkFail {
		return checks
	}
	checks = append(checks, doctorKey(cfg, status.Runner), doctorVersion(version.Get().Version, status))
	return append(checks, doctorSources(settings, status.Runner.Sources)...)
}

func doctorSettings(lookup config.LookupFunc) (config.LookupFunc, config.Config, doctorCheck) {
	const name = "Settings"
	settings, err := config.WithEnvFile(lookup, config.RunnerEnvFile)
	if err != nil {
		return nil, config.Config{}, doctorCheck{checkFail, name, err.Error()}
	}
	cfg, err := config.Load(settings)
	if err != nil {
		return nil, config.Config{}, doctorCheck{checkFail, name, strings.ReplaceAll(err.Error(), "\n", "; ")}
	}
	if !cfg.Token.IsSet() || !cfg.RunnerKey.IsSet() {
		return nil, config.Config{}, doctorCheck{checkFail, name, "this runner isn't registered: run backproof register with an install token from the dashboard's Add runner page, or set " + config.EnvToken + " to a CI token"}
	}
	if _, err := crypto.RunnerPublicKey(cfg.RunnerKey); err != nil {
		return nil, config.Config{}, doctorCheck{checkFail, name, config.EnvRunnerKey + " is not valid: register this runner again"}
	}
	return settings, cfg, doctorCheck{checkOK, name, "valid, using the API at " + cfg.APIURL}
}

func doctorDocker(ctx context.Context) doctorCheck {
	const name = "Docker"
	unreachable := func(err error) doctorCheck {
		return doctorCheck{checkFail, name, fmt.Sprintf("can't reach Docker (%v). Check that Docker is running, and mount /var/run/docker.sock when the runner runs in a container.", err)}
	}

	client, err := docker.New("")
	if err != nil {
		return unreachable(err)
	}
	defer func() { _ = client.Close() }()

	ctx, cancel := context.WithTimeout(ctx, doctorDockerTimeout)
	defer cancel()
	if err := client.Ping(ctx); err != nil {
		return unreachable(err)
	}
	return doctorCheck{checkOK, name, "reachable"}
}

func doctorAPI(ctx context.Context, cfg config.Config) (api.StatusResult, doctorCheck) {
	const name = "Backproof API"
	client, err := api.NewAuthorized(cfg.APIURL, version.Get().Version, cfg.Token)
	if err != nil {
		return api.StatusResult{}, doctorCheck{checkFail, name, err.Error()}
	}
	status, err := client.Status(ctx)
	var problem *api.ResponseError
	switch {
	case errors.As(err, &problem):
		return api.StatusResult{}, doctorCheck{checkFail, name, problem.Message}
	case err != nil:
		return api.StatusResult{}, doctorCheck{checkFail, name, err.Error() + ". Check that this host can make outbound HTTPS requests."}
	}
	return status, doctorCheck{checkOK, name, fmt.Sprintf("connected as runner %q", status.Runner.Name)}
}

func doctorKey(cfg config.Config, runner api.RunnerStatus) doctorCheck {
	const name = "Runner key"
	publicKey, err := crypto.RunnerPublicKey(cfg.RunnerKey)
	if err != nil {
		return doctorCheck{checkFail, name, config.EnvRunnerKey + " is not valid: register this runner again"}
	}
	fingerprint := crypto.KeyFingerprint(publicKey)
	if fingerprint != runner.KeyFingerprint {
		return doctorCheck{checkFail, name, fmt.Sprintf("this runner's key %s doesn't match the key %s that Backproof holds, so it can't open keys entered in the dashboard. Register the runner again, or create a new CI token.", fingerprint, runner.KeyFingerprint)}
	}
	return doctorCheck{checkOK, name, fingerprint + " matches the dashboard"}
}

func doctorVersion(current string, status api.StatusResult) doctorCheck {
	const name = "Version"
	// semver needs the v prefix, which runner versions don't carry.
	if !semver.IsValid("v" + current) {
		return doctorCheck{checkOK, name, current + " is a development build, so it isn't compared with releases"}
	}
	if minimum := status.MinVersion; semver.IsValid("v"+minimum) && semver.Compare("v"+current, "v"+minimum) < 0 {
		return doctorCheck{checkFail, name, fmt.Sprintf("%s is below the minimum version %s, so this runner gets no jobs. Update it with: %s", current, minimum, updateCommand)}
	}
	if latest := status.LatestVersion; semver.IsValid("v"+latest) && semver.Compare("v"+current, "v"+latest) < 0 {
		return doctorCheck{checkWarn, name, fmt.Sprintf("%s is installed and %s is available. Update with: %s", current, latest, updateCommand)}
	}
	return doctorCheck{checkOK, name, current + " is up to date"}
}

func doctorSources(settings config.LookupFunc, sources []api.RunnerSource) []doctorCheck {
	if len(sources) == 0 {
		return []doctorCheck{{checkOK, "Sources", "none yet. Add a source in the dashboard."}}
	}
	lookup, err := config.WithEnvFile(settings, config.SourcesEnvFile)
	if err != nil {
		return []doctorCheck{{checkFail, "Sources", err.Error()}}
	}

	checks := make([]doctorCheck, 0, len(sources))
	for _, source := range sources {
		checks = append(checks, doctorSource(source, config.LoadSourceSecrets(lookup, source.Id)))
	}
	return checks
}

// doctorSource reports where a source's keys come from, never the keys themselves.
func doctorSource(source api.RunnerSource, local config.SourceSecrets) doctorCheck {
	name := "Source " + source.Id
	accessKey, secretKey := config.EnvSourceS3AccessKey(source.Id), config.EnvSourceS3SecretKey(source.Id)
	hasAccessKey, hasSecretKey := local.S3AccessKey.IsSet(), local.S3SecretKey.IsSet()

	switch {
	case hasAccessKey && hasSecretKey && source.HasSealedKeys:
		return doctorCheck{checkOK, name, "local keys, which take precedence over the keys entered in the dashboard"}
	case hasAccessKey && hasSecretKey:
		return doctorCheck{checkOK, name, "local keys"}
	case hasAccessKey != hasSecretKey:
		return doctorCheck{checkWarn, name, fmt.Sprintf("only one of %s and %s is set on this runner. Set both, or remove it to use the keys entered in the dashboard.", accessKey, secretKey)}
	case source.HasSealedKeys:
		return doctorCheck{checkOK, name, "keys entered in the dashboard, sealed for this runner"}
	default:
		return doctorCheck{checkWarn, name, fmt.Sprintf("no keys on this runner, so its tests can't run here. Enter its keys in the dashboard, or set %s and %s in %s.", accessKey, secretKey, config.SourcesEnvFile)}
	}
}
