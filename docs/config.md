# Configuration

The runner is configured through environment variables. An empty variable counts as not set. If a setting is invalid, the runner names it, says what to change and stops.

## Runner settings

| Variable | Purpose | Default | Valid values |
| --- | --- | --- | --- |
| `BACKPROOF_TOKEN` | Runner token | None | Secret |
| `BACKPROOF_RUNNER_KEY` | The runner's private age key, created at install | None | Secret |
| `BACKPROOF_API_URL` | Address of the Backproof API | `https://app.backproof.dev` | An `https://` address without credentials, a query or a fragment. `http://` is accepted only for `localhost` and loopback addresses, for local development |
| `BACKPROOF_NAME` | Runner name shown in the dashboard | None | Any text |
| `BACKPROOF_CONCURRENCY` | Jobs at a time | `1` | A whole number of 1 or more |
| `BACKPROOF_WORK_DIR` | Directory for temporary files | `/var/lib/backproof` | An absolute path |
| `BACKPROOF_LOG_LEVEL` | How much the runner logs | `info` | `debug`, `info`, `warn` or `error` |

CI users only set `BACKPROOF_TOKEN`.

## Files

> **Not read yet.** The runner currently reads environment variables only. It starts reading these files when registration and agent mode are added.

| File | Purpose |
| --- | --- |
| `/etc/backproof/runner.env` | The runner settings above, written by the install script with mode `600` |
| `/etc/backproof/sources.env` | Optional local secrets for backup sources |

## Local secrets

> **Not read yet.** These variables take effect when backup sources are added.

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
