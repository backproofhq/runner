# Changelog

All notable changes to the Backproof runner are documented here. The project follows [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## Unreleased

## 0.1.0-beta.1 - 2026-11-24

The first pre-release of the Backproof runner. It proves the release pipeline and lets beta testers install on purpose with `--version 0.1.0-beta.1`. `latest` doesn't point at it.

### Added

- `scripts/install.sh` installs the runner with one command: `curl -fsSL https://get.backproof.dev | sh -s -- --token bp_inst_...`. It offers to install Docker, registers the runner, starts it and waits for its first check-in. It takes `--name`, `--version`, `--drain-timeout`, `--yes`, `--dry-run` and `--uninstall`.
- Running the install script again updates the runner. It lets a running backup test finish, keeps the runner's settings, asks before a new major version and puts the previous version back if the new one doesn't check in.
- `backproof register --token` registers a runner with an install token, creates its key pair and saves its settings to `/etc/backproof/runner.env`.
- `backproof agent` polls Backproof for due jobs, tests the latest backup and reports the result. It finishes running jobs before it stops.
- `backproof run --once` runs the jobs that are due and exits with `0` when all passed, `1` when a backup failed and `2` when the runner couldn't do its job.
- CI jobs need one secret: `BACKPROOF_TOKEN` accepts a CI token that also holds the runner's private key, which never leaves the job.
- A GitLab CI template, `ci/gitlab/backproof.gitlab-ci.yml`, that runs due backup tests on a pipeline schedule.
- `backproof doctor` checks the settings, Docker, the token, the runner's key and version, and which sources the runner has keys for. It never prints a key and exits with `2` when a check fails.
- The runner finds the newest matching backup in S3-compatible storage, checks that it fits on disk and downloads it. `BACKPROOF_DISK_MARGIN` sets how much free space is required.
- Backups can be plain SQL or custom-format dumps from Postgres 14 to 18, compressed with gzip or zstd and encrypted with age or GPG. Each one is restored in a throwaway container with no network access.
- Backup checks fail a restored backup when tables are missing, row counts move past their tolerance or the newest row is too old.
- Table names leave the runner hashed, unless a source allows plain names.
- The runner opens storage and decryption keys sealed for it in the dashboard. Local secrets take precedence over sealed keys.
- `@backproof/seal`, the open-source package the dashboard uses to seal keys in the browser.
- Containers and volumes left behind by a killed run are removed the next time the runner starts.
- Validated settings from `BACKPROOF_*` environment variables, with structured JSON logs. `BACKPROOF_MEMORY_LIMIT` and `BACKPROOF_CPU_LIMIT` limit the throwaway Postgres containers.
- The v1 API contract between the runner and Backproof, in `api/v1/openapi.yaml`. The runner uses `https://api.backproof.dev` by default.
- A development command, `backproof restore --file`, that restores a local dump and prints the result as JSON.
- Releases publish the `backproofhq/runner` image for Linux on amd64 and arm64, binaries, `install.sh`, checksums and SBOMs. `checksums.txt` and the images are signed with cosign by the release workflow. `latest` and the short version tags move only on stable releases.
- `docs/install.md` covers installing, updating and removing the runner, running it from cron or CI, and verifying a download.
