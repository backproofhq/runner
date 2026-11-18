# Configuration

The runner is configured through environment variables. An empty variable counts as not set. If a setting is invalid, the runner names it, says what to change and stops.

## Runner settings

| Variable | Purpose | Default | Valid values |
| --- | --- | --- | --- |
| `BACKPROOF_TOKEN` | Runner token | None | Secret |
| `BACKPROOF_RUNNER_KEY` | The runner's private age key, created at install | None | Secret |
| `BACKPROOF_API_URL` | Address of the Backproof API | `https://api.backproof.dev` | An `https://` address without credentials, a query or a fragment. `http://` is accepted only for `localhost` and loopback addresses, for local development |
| `BACKPROOF_NAME` | Runner name shown in the dashboard | None | Any text |
| `BACKPROOF_CONCURRENCY` | Jobs at a time | `1` | A whole number of 1 or more |
| `BACKPROOF_WORK_DIR` | Directory for temporary files, including unpacked backups. They are deleted when the job ends | `/var/lib/backproof` | An absolute path |
| `BACKPROOF_LOG_LEVEL` | How much the runner logs | `info` | `debug`, `info`, `warn` or `error` |
| `BACKPROOF_MEMORY_LIMIT` | Memory each throwaway Postgres container may use, with no swap | No limit | A whole number of bytes, or a number followed by `k`, `m`, `g` or `t`. At least `64m` |
| `BACKPROOF_CPU_LIMIT` | CPUs each throwaway Postgres container may use | No limit | A number of `0.01` or more, such as `1.5` |
| `BACKPROOF_DISK_MARGIN` | How many times the backup's size must be free in the work directory before it is downloaded. Leave room for decompression | `3` | A number from `1` to `1000` |

CI users only set `BACKPROOF_TOKEN`.

## Files

> The runner reads environment variables. `backproof register` writes `runner.env`, which is passed to the container with `--env-file`. The runner doesn't read `sources.env` yet.

| File | Purpose |
| --- | --- |
| `/etc/backproof/runner.env` | The runner settings above, written by `backproof register` with mode `600` |
| `/etc/backproof/sources.env` | Optional local secrets for backup sources |

## Local secrets

> **Partly read.** The development command `backproof restore --file` reads the age and GPG variables with the source ID `LOCAL`, for example `BP_SRC_LOCAL_AGE_IDENTITY`. The other variables take effect when backup sources are added.

Teams that want no secret to pass through Backproof can set these in `/etc/backproof/sources.env`, or as CI secrets with the same names. Local secrets take precedence over keys sealed in the dashboard.

| Variable | Purpose |
| --- | --- |
| `BP_SRC_<ID>_S3_ACCESS_KEY` | Read-only access key for the source's bucket |
| `BP_SRC_<ID>_S3_SECRET_KEY` | Secret key for the source's bucket |
| `BP_SRC_<ID>_AGE_IDENTITY` | age private key, if the backups use age |
| `BP_SRC_<ID>_GPG_KEY` | GPG private key, if the backups use GPG |
| `BP_SRC_<ID>_GPG_PASSPHRASE` | Passphrase for the GPG key |
| `BP_SRC_<ID>_NAME_SALT` | Salt for hashing table names. The dashboard shows the value to paste |

`<ID>` is the source ID in upper case, for example `BP_SRC_PROD_DB_S3_ACCESS_KEY`.
