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
- `backproof register --token` registers a runner with an install token, creates its key pair and saves its settings to `/etc/backproof/runner.env`.
- The v1 API contract between the runner and Backproof, in `api/v1/openapi.yaml`. The runner uses `https://api.backproof.dev` by default.
- `BACKPROOF_MEMORY_LIMIT` and `BACKPROOF_CPU_LIMIT` settings for the throwaway Postgres containers.
