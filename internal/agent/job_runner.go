package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"
	"unicode/utf8"

	"github.com/backproofhq/runner/internal/api"
	"github.com/backproofhq/runner/internal/archive"
	"github.com/backproofhq/runner/internal/config"
	"github.com/backproofhq/runner/internal/crypto"
	"github.com/backproofhq/runner/internal/engine/postgres"
	"github.com/backproofhq/runner/internal/job"
	"github.com/backproofhq/runner/internal/source"
)

const maxReasonLength = 1000

// JobRunnerOptions configures how claimed jobs are run.
type JobRunnerOptions struct {
	Engine      job.Engine
	Logger      *slog.Logger
	WorkDir     string
	DiskMargin  float64
	Lookup      config.LookupFunc
	SourcesFile string
	// RunnerKey opens the keys sealed for this runner in the dashboard.
	RunnerKey config.Secret
}

// NewJobRunner returns the Runner that tests a claimed job's latest backup.
func NewJobRunner(options JobRunnerOptions) Runner {
	return func(ctx context.Context, assigned api.Job, jobContext JobContext) api.JobResult {
		// Read for every job, so changed secrets work without a restart.
		lookup, err := config.WithEnvFile(options.Lookup, options.SourcesFile)
		if err != nil {
			options.Logger.ErrorContext(ctx, "read local secrets", slog.Any("error", err))
			return ToResult(job.ErrorResult(job.RunnerError("sources_file_invalid", "The runner can't read "+options.SourcesFile+". Correct the file and try again.", nil)), job.PlainTableKey)
		}

		secrets, failure := jobSecrets(config.LoadSourceSecrets(lookup, assigned.Source.Id), assigned, options.RunnerKey)
		if failure != nil {
			return ToResult(job.ErrorResult(failure), job.PlainTableKey)
		}

		tableKey := job.PlainTableKey
		if !assigned.Source.ShowTableNames {
			tableKey = job.HashedTableKey(secrets.NameSalt)
		}

		backups := source.New(sourceConfig(assigned.Source, secrets))
		result := job.RunSource(ctx, options.Engine, backups, options.Logger, assigned.Id.String(), job.Options{
			Checks:       checksFromJob(assigned, tableKey),
			WorkDir:      options.WorkDir,
			SourceID:     assigned.Source.Id,
			Secrets:      secrets,
			DiskMargin:   options.DiskMargin,
			PostgresHint: postgresHint(assigned.Source),
			OnStage:      jobContext.OnStage,
			RunningJobs:  jobContext.RunningJobs,
		})
		return ToResult(result, tableKey)
	}
}

// jobSecrets combines the local secrets with the keys sealed for this runner. Local secrets take
// precedence, so a sealed bundle this runner can't open only matters when something is missing.
// The result lives in memory for this one job.
func jobSecrets(local config.SourceSecrets, assigned api.Job, runnerKey config.Secret) (config.SourceSecrets, *job.Failure) {
	secrets := local
	var sealedProblem *job.Failure
	if assigned.SealedBundle != nil {
		sealed, err := crypto.OpenSealedBundle(*assigned.SealedBundle, runnerKey)
		switch {
		case err == nil:
			secrets = withSealedKeys(local, sealed)
		case errors.Is(err, crypto.ErrWrongKey):
			// Reported below as missing credentials, because the fix is to enter the keys for this runner.
		case errors.Is(err, crypto.ErrNoKey), errors.Is(err, crypto.ErrInvalidKey):
			sealedProblem = job.RunnerError("invalid_runner_key", "This runner's private key can't be used. Register the runner again, then enter the keys for its sources in the dashboard.", nil)
		default:
			sealedProblem = job.RunnerError("sealed_keys_invalid", fmt.Sprintf("The sealed keys for source %s can't be read. Enter them again in the dashboard.", assigned.Source.Id), nil)
		}
	}

	if hasRequiredSecrets(secrets, assigned.Source) {
		return secrets, nil
	}
	if sealedProblem != nil {
		return config.SourceSecrets{}, sealedProblem
	}
	return config.SourceSecrets{}, job.RunnerError(
		"missing_credentials",
		fmt.Sprintf("No credentials for source %s on this runner. Enter its keys in the dashboard.", assigned.Source.Id),
		nil,
	)
}

func hasRequiredSecrets(secrets config.SourceSecrets, assigned api.Source) bool {
	return secrets.S3AccessKey.IsSet() && secrets.S3SecretKey.IsSet() && (assigned.ShowTableNames || secrets.NameSalt.IsSet())
}

// withSealedKeys fills what is missing locally. Keys that belong together always come from one place.
func withSealedKeys(local config.SourceSecrets, sealed crypto.SealedBundle) config.SourceSecrets {
	secrets := local
	if !local.S3AccessKey.IsSet() || !local.S3SecretKey.IsSet() {
		secrets.S3AccessKey, secrets.S3SecretKey = sealed.S3AccessKey, sealed.S3SecretKey
	}
	if !local.GPGKey.IsSet() {
		secrets.GPGKey, secrets.GPGPassphrase = sealed.GPGKey, sealed.GPGPassphrase
	}
	if !local.AgeIdentity.IsSet() {
		secrets.AgeIdentity = sealed.AgeIdentity
	}
	if !local.NameSalt.IsSet() {
		secrets.NameSalt = sealed.NameSalt
	}
	return secrets
}

// DefaultCapabilities lists what this build of the runner can do.
func DefaultCapabilities() api.Capabilities {
	majors := make([]int, 0, postgres.NewestMajor-postgres.OldestMajor+1)
	for major := postgres.OldestMajor; major <= postgres.NewestMajor; major++ {
		majors = append(majors, major)
	}
	return api.Capabilities{
		PostgresMajors: majors,
		Compression:    []api.Compression{api.CompressionGzip, api.CompressionZstd},
		Encryption:     []api.Encryption{api.EncryptionAge, api.EncryptionGpg},
	}
}

func sourceConfig(assigned api.Source, secrets config.SourceSecrets) source.Config {
	cfg := source.Config{
		ID:        assigned.Id,
		Endpoint:  assigned.Endpoint,
		Bucket:    assigned.Bucket,
		Prefix:    assigned.Prefix,
		Pattern:   assigned.Pattern,
		AccessKey: secrets.S3AccessKey,
		SecretKey: secrets.S3SecretKey,
	}
	if assigned.Region != nil {
		cfg.Region = *assigned.Region
	}
	if assigned.PinnedObjectKey != nil {
		cfg.ObjectKey = *assigned.PinnedObjectKey
	}
	return cfg
}

func postgresHint(assigned api.Source) int {
	if assigned.PostgresVersionHint == nil {
		return 0
	}
	return *assigned.PostgresVersionHint
}

func checksFromJob(assigned api.Job, tableKey job.TableKeyFunc) job.Checks {
	config := assigned.Checks
	checks := job.Checks{
		Baseline:              make([]job.KeyedCount, 0, len(config.Baseline)),
		TolerancePercent:      float64(config.TolerancePercent),
		TableTolerancePercent: make(map[string]float64, len(config.TableTolerances)),
		EstimateRows:          config.RowCountMode == api.Estimate,
		TableKey:              tableKey,
	}
	// A list of readable names can't be compared with hashed keys.
	if assigned.Source.ShowTableNames {
		checks.ExpectedTables = config.ExpectedTables
	}
	for _, table := range config.Baseline {
		checks.Baseline = append(checks.Baseline, job.KeyedCount{Key: table.TableKey, Rows: table.Rows})
	}
	for _, table := range config.TableTolerances {
		checks.TableTolerancePercent[table.TableKey] = float64(table.TolerancePercent)
	}
	if latest := config.LatestTimestamp; latest != nil {
		checks.LatestTimestamp = &job.TimestampCheck{
			Table:  job.TableRef{Schema: latest.Schema, Name: latest.Table},
			Column: latest.Column,
			MaxAge: time.Duration(latest.MaxAgeSeconds) * time.Second,
		}
	}
	return checks
}

// ToResult turns a job's result into what is sent to Backproof. docs/payload.md lists every field.
func ToResult(result job.Result, tableKey job.TableKeyFunc) api.JobResult {
	converted := api.JobResult{
		SchemaVersion: api.JobResultSchemaVersion(result.SchemaVersion),
		Status:        api.JobResultStatus(result.Status),
		ReasonCode:    optional(result.ReasonCode),
		Reason:        optional(shorten(result.Reason)),
		Backup:        backupDetails(result.Backup),
		Stages:        make([]api.StageTiming, 0, len(result.Stages)),
		Checks:        make([]api.CheckResult, 0, len(result.Checks)),
		Tables:        make([]api.TableRowCount, 0, len(result.Tables)),
		Notes:         make([]string, 0, len(result.Notes)),
	}
	if result.Stage != "" {
		stage := api.JobResultFailedStage(result.Stage)
		converted.FailedStage = &stage
	}
	for _, stage := range result.Stages {
		converted.Stages = append(converted.Stages, api.StageTiming{Name: api.Stage(stage.Name), DurationMs: stage.DurationMS})
	}
	for _, check := range result.Checks {
		converted.Checks = append(converted.Checks, checkResult(check))
	}
	for _, table := range result.Tables {
		key := tableKey(job.TableRef{Schema: table.Schema, Name: table.Name})
		converted.Tables = append(converted.Tables, api.TableRowCount{TableKey: key, Rows: table.Rows})
	}
	for _, note := range result.Notes {
		converted.Notes = append(converted.Notes, shorten(note))
	}
	return converted
}

func backupDetails(backup job.Backup) api.BackupDetails {
	details := api.BackupDetails{
		ObjectKey:       optional(backup.ObjectKey),
		PostgresVersion: optional(backup.PostgresVersion),
	}
	if backup.ObjectKey != "" {
		details.SizeBytes = &backup.SizeBytes
	}
	if !backup.Time.IsZero() {
		details.Time = &backup.Time
	}
	if backup.PostgresMajor > 0 {
		details.PostgresMajor = &backup.PostgresMajor
	}
	if backup.RestoredMajor > 0 {
		details.RestoredMajor = &backup.RestoredMajor
	}
	if backup.VersionSource != "" {
		source := api.BackupDetailsVersionSource(backup.VersionSource)
		details.VersionSource = &source
	}
	if backup.Format != "" {
		format := api.BackupDetailsFormatUnknown
		if backup.Format == archive.FormatCustom || backup.Format == archive.FormatPlain {
			format = api.BackupDetailsFormat(backup.Format)
		}
		details.Format = &format
	}
	if backup.Compression != "" {
		compression := api.BackupDetailsCompression(backup.Compression)
		details.Compression = &compression
	}
	switch backup.Encryption {
	case string(archive.FormatAge):
		encryption := api.BackupDetailsEncryptionAge
		details.Encryption = &encryption
	case string(archive.FormatPGP):
		encryption := api.BackupDetailsEncryptionGpg
		details.Encryption = &encryption
	}
	return details
}

func checkResult(check job.CheckResult) api.CheckResult {
	converted := api.CheckResult{
		Name:     api.CheckResultName(check.Name),
		Status:   api.CheckResultStatus(check.Status),
		Code:     optional(check.Code),
		Reason:   optional(shorten(check.Reason)),
		Measured: optional(check.Measured),
		Expected: optional(check.Expected),
		Tables:   make([]api.TableFinding, 0, len(check.Tables)),
	}
	for _, finding := range check.Tables {
		converted.Tables = append(converted.Tables, api.TableFinding{
			TableKey:         finding.Key,
			Problem:          api.TableFindingProblem(finding.Problem),
			MeasuredRows:     finding.MeasuredRows,
			ExpectedRows:     finding.ExpectedRows,
			ChangePercent:    optionalFloat(finding.ChangePercent),
			TolerancePercent: optionalFloat(finding.TolerancePercent),
		})
	}
	return converted
}

func optional(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}

func optionalFloat(value *float64) *float32 {
	if value == nil {
		return nil
	}
	converted := float32(*value)
	return &converted
}

func shorten(text string) string {
	if utf8.RuneCountInString(text) <= maxReasonLength {
		return text
	}
	return string([]rune(text)[:maxReasonLength])
}
