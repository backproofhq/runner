# Changelog

All notable changes to the Backproof runner are documented here. The project follows [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## Unreleased

### Added

- The `backproof` command with `agent`, `run --once`, `register`, `doctor` and `version`. Only `version` works so far.
- Validated settings from `BACKPROOF_*` environment variables, with structured JSON logs.
- Docker image and snapshot builds for Linux on amd64 and arm64.
- A development command, `backproof restore --file`, that restores a plain SQL or custom-format dump from Postgres 14 to 18 in a throwaway container and prints the result as JSON.
- Backup checks that fail a restored backup when tables are missing, row counts move past their tolerance or the newest row is too old.
- `BACKPROOF_MEMORY_LIMIT` and `BACKPROOF_CPU_LIMIT` settings for the throwaway Postgres containers.
