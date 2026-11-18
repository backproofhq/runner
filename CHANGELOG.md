# Changelog

All notable changes to the Backproof runner are documented here. The project follows [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## Unreleased

### Added

- The `backproof` command with `agent`, `run --once`, `register`, `doctor` and `version`. Only `version` works so far.
- Validated settings from `BACKPROOF_*` environment variables, with structured JSON logs.
- Docker image and snapshot builds for Linux on amd64 and arm64.
- A development command, `backproof restore --file`, that restores a plain SQL or custom-format dump from Postgres 14 to 18 in a throwaway container and prints the result as JSON.
- Backup checks that fail a restored backup when tables are missing, row counts move past their tolerance or the newest row is too old.
- `backproof restore --file` opens backups compressed with gzip or zstd and encrypted with age or GPG.
- The runner can find the newest matching backup in S3-compatible storage, check that it fits on disk and download it. `BACKPROOF_DISK_MARGIN` sets how much free space is required.
- Containers and volumes left behind by a killed run are removed the next time the runner starts.
- `backproof run --once` runs the jobs that are due and exits with `0` when all passed, `1` when a backup failed and `2` when the runner couldn't do its job.
- CI jobs need one secret: `BACKPROOF_TOKEN` accepts a CI token that also holds the runner's private key, which never leaves the job.
- The agent opens keys sealed for it in the dashboard. Local secrets take precedence over sealed keys.
- `backproof agent` polls Backproof for due jobs, tests the latest backup and reports the result. It finishes running jobs before it stops, and table names leave the runner hashed unless a source allows plain names.
- `backproof register --token` registers a runner with an install token, creates its key pair and saves its settings to `/etc/backproof/runner.env`.
- The v1 API contract between the runner and Backproof, in `api/v1/openapi.yaml`. The runner uses `https://api.backproof.dev` by default.
- `BACKPROOF_MEMORY_LIMIT` and `BACKPROOF_CPU_LIMIT` settings for the throwaway Postgres containers.
- `@backproof/seal`, the open-source package the dashboard uses to seal storage and decryption keys in the browser, and the runner code that opens them.
