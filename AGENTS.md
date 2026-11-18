# Backproof Runner

Instructions for coding agents working in `backproofhq/runner`. Read this file fully before every task.

## What this repo is

The Backproof runner: one open-source Go binary, shipped as the Docker image `backproofhq/runner`. It runs in the customer's infrastructure and restores their latest Postgres backup into a throwaway container, runs checks on it and reports metadata to the Backproof API. Customer data never leaves their machine. People trust us with their backups, so correctness and safety come before speed.

License: Apache 2.0.

## Sources of truth

Read the relevant part before you change anything. If the code and these documents disagree, stop and ask; don't pick one yourself.

| File | What it defines |
| --- | --- |
| `api/v1/openapi.yaml` | The runner ↔ API contract |
| `docs/payload.md` | Every field that leaves the customer's infrastructure |
| `docs/config.md` | Environment variables and files the runner reads |
| `docs/security.md` | Trust model, sealed secrets, what we promise users |
| `CHANGELOG.md` | Released and unreleased changes |

## How to work

1. Work per milestone at a time, in order. Don't start the next one until the current one's "done when" items pass.
2. Before writing code, say in a few lines which milestone item you are working on and how you'll approach it.
3. Keep changes small: one logical change per commit, one milestone item (or less) per pull request.
4. Run the checks below before you say a task is done. Report what you ran and the result.
5. If something is unclear, missing from the plan or seems wrong, stop and ask. Never invent API fields, payload fields, config variables or CLI flags.

## Plans

Do not commit internal implementation plans & research notes. Keep temporart work outside the worktree. `.plans/` is git ingored and can be used for plans per specific feature.

## Commands

```sh
go build ./...                                    # build
go test ./...                                     # unit tests
go test -tags integration ./test/...              # integration tests (needs Docker)
golangci-lint run                                 # lint
gofmt -l .                                        # must print nothing
shellcheck scripts/install.sh                     # install script lint
goreleaser release --snapshot --clean             # local release build, never publishes
```

All of these must pass before a pull request. Never skip, disable or weaken a test or lint rule to make a check pass.

## Repository layout

```text
cmd/backproof/          CLI entry point (cobra)
internal/agent/         poll loop, drain, graceful shutdown
internal/api/           v1 client and types, generated from api/v1/openapi.yaml
internal/job/           the eight pipeline stages and the result builder
internal/source/        S3 listing and download
internal/archive/       format sniffing, gzip, zstd
internal/crypto/        age and GPG decryption, sealed-bundle opening
internal/engine/postgres/  version detection, restore, checks (behind the Engine interface)
internal/docker/        containers, labels, orphan cleanup
internal/config/        env loading and validation
internal/version/       build version, set by GoReleaser
packages/seal/          TypeScript sealing package used by the dashboard
scripts/install.sh      install and update script
ci/gitlab/              GitLab CI template
test/                   integration tests and fixture generators
```

New code goes where its responsibility already lives. Ask before adding a new top-level package.

## Hard rules

These protect users' data and trust. Breaking one is never acceptable, even to make a test pass.

**Secrets**

- Never open, print, copy or summarise secret files or values: `.env*`, `runner.env`, `sources.env`, anything under `/etc/backproof/`, private keys, tokens or passphrases. This includes your own environment variables.
- Test keys and tokens are generated at test time. Never commit a real or hardcoded key.
- Decrypted secrets live in memory for one job only. Never write them to disk, log them or put them in an error message.

**Customer data**

- Never log, print or report row contents or SQL from a dump. Logs may contain table names, counts, sizes and timings.
- Table names leave the runner hashed, unless the job says the source allows plain names.
- Anything that changes what leaves the runner updates `docs/payload.md` and bumps `schema_version` in the same pull request.

**API**

- Change `api/v1/openapi.yaml` first, then regenerate the client, then write code.
- v1 changes are additions only: no renamed, removed or retyped fields.

**Containers**

- Every container and volume the runner creates carries the labels `dev.backproof.runner` and `dev.backproof.job`, including in tests.
- Throwaway Postgres containers run with network `none` and no published ports.
- Every code path, including errors, timeouts and panics, cleans up what it created.

**Install script**

- `scripts/install.sh` is POSIX `sh` with `set -eu`, clean under `shellcheck`.
- Prompts read from `/dev/tty`. Every prompt has a `--yes` path and is described by `--dry-run`.

## Code style

- Standard Go: `gofmt`, idiomatic names, small packages with clear jobs.
- Pass `context.Context` as the first argument to anything that does I/O or can take time.
- Wrap errors with `%w` and context: `fmt.Errorf("download %s: %w", key, err)`.
- Use `log/slog` with structured fields. No `fmt.Println` for logging.
- No package-level mutable state. Pass dependencies in.
- Prefer the standard library. Add a dependency only when the standard library or an existing one can't do the job, and say why in the pull request.
- Comments only where code can't speak for itself: the why behind a non-obvious decision, a workaround, a security reason. Don't restate what the code does.
- Error messages for users say what happened and what to do next, for example: "No credentials for source PROD_DB on this runner. Enter its keys in the dashboard."

## Results: fail versus error

Get this right everywhere; alerts depend on it.

- `fail`: the backup is bad. It won't decrypt, decompress or restore, or a check failed.
- `error`: the runner couldn't do its job. Examples: wrong credentials, Docker unreachable, disk full.

## Testing & Verifying

- Every bug in restoring or checking gets a fixture test that reproduces it before the fix.
- Fixtures are generated at test time with `pg_dump` from each supported Postgres major. Don't commit large dump files.
- Integration tests use `testcontainers-go` with real `postgres:<major>` images and MinIO.
- Each broken-backup fixture asserts its expected result (`fail` or `error`), its stage and a readable reason.
- After each test, no container or volume with the runner label may remain.

## Commits and pull requests

**Commits** follow Conventional Commits, with a scope from the layout above:

```text
feat(engine/postgres): detect version from plain SQL header
fix(docker): remove orphaned volumes after a killed job
test(archive): add truncated zstd fixture
docs(payload): document per-table row counts
```

Descriptions of commits should be consice or not exists at all

- Types: `feat`, `fix`, `test`, `docs`, `refactor`, `perf`, `build`, `ci`, `chore`.
- A breaking change gets `!` after the type and a `BREAKING CHANGE:` footer.
- Sign off every commit (`git commit -s`) for the Developer Certificate of Origin.

**Pull requests** use this structure:

- Conventional commit titles, plain language: fix(web): new threads no longer spike CPU.
- Body: the problem in a sentence or two, then how you fixed it.
- Only when asked should you split a PR into stacked PR.
- Watch the PR untill al checks are verfied, if any pre-pr check failed fix them and update the PR.

When an pull requests implements or add additions to an existing pull request or issue of someone else, make sure to mention them and give them credit.

## Never do this

- Push to `main`, create tags or publish releases. A human does that.
- Edit released sections of `CHANGELOG.md`.
- Change the license, `SECURITY.md` or `docs/security.md` without being asked.
- Turn off, skip or loosen tests, lint rules or CI steps.
- Make network calls to real customer storage or the production API from tests.