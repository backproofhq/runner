# Changelog

All notable changes to the Backproof runner are documented here. The project follows [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## Unreleased

### Added

- The `backproof` command with `agent`, `run --once`, `register`, `doctor` and `version`. Only `version` works so far.
- Validated settings from `BACKPROOF_*` environment variables, with structured JSON logs.
- Docker image and snapshot builds for Linux on amd64 and arm64.
