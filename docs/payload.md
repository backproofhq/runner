# Payload

This document lists every field that leaves your infrastructure when you run the Backproof runner. [security.md](security.md) describes what the runner promises never to send.

## Registration

`backproof register` sends one request to `POST /v1/runners/register`.

| Field | What it is | Example |
| --- | --- | --- |
| `install_token` | The one-time token from the dashboard's Add runner page | `bp_inst_...` |
| `name` | The runner's name: `BACKPROOF_NAME`, or the hostname when that isn't set | `build-box` |
| `public_key` | The public half of the runner's age key. The private key never leaves the runner | `age1...` |
| `os` | The operating system | `linux` |
| `arch` | The processor architecture | `amd64` |

Every request also has a `User-Agent` header with the runner's version, such as `backproof-runner/1.2.3`.

## Check-ins

`backproof agent` sends `POST /v1/runners/poll` about every 30 seconds.

| Field | What it is | Example |
| --- | --- | --- |
| `version` | The exact runner version | `1.2.3` |
| `status` | Whether the runner can take a job | `idle` or `busy` |
| `capabilities.postgres_majors` | The Postgres versions it can restore into | `[14, 15, 16, 17, 18]` |
| `capabilities.compression` | The compression formats it can open | `["gzip", "zstd"]` |
| `capabilities.encryption` | The encryption formats it can open | `["age", "gpg"]` |

## Heartbeats

While a job runs, the runner sends `POST /v1/jobs/{id}/heartbeat` every 60 seconds.

| Field | What it is | Example |
| --- | --- | --- |
| `stage` | The stage the job is in | `restore` |

## Job results

When a job ends, the runner sends `POST /v1/jobs/{id}/result` once. Results use `schema_version` 1.

| Field | What it is | Example |
| --- | --- | --- |
| `schema_version` | The version of this format | `1` |
| `status` | `pass`, `fail` for a bad backup, or `error` when the runner couldn't do its job | `fail` |
| `failed_stage` | The stage that stopped the job | `restore` |
| `reason_code` | A stable code for the outcome | `missing_extension` |
| `reason` | A short summary. It never contains SQL, row contents, raw tool output or table names | `The backup needs the Postgres extension "postgis"...` |
| `backup.object_key` | The backup's key in your bucket | `prod/daily.dump.gz` |
| `backup.size_bytes` | The size of the stored backup | `1048576` |
| `backup.time` | When the backup was stored | `2026-11-18T02:00:00Z` |
| `backup.format` | `plain`, `custom` or `unknown` | `custom` |
| `backup.compression` | `gzip` or `zstd` | `gzip` |
| `backup.encryption` | `age` or `gpg` | `age` |
| `backup.postgres_version` | The Postgres version that made the backup | `17.4` |
| `backup.postgres_major` | Its major version | `17` |
| `backup.version_source` | Where that version came from: `dump`, `hint` or `default` | `dump` |
| `backup.restored_major` | The Postgres major the backup was restored into | `17` |
| `stages[].name` | A stage that ran | `download` |
| `stages[].duration_ms` | How long it took | `5300` |
| `checks[].name` | `tables_exist`, `row_counts` or `latest_timestamp` | `row_counts` |
| `checks[].status` | `pass`, `fail` or `skipped` | `pass` |
| `checks[].code` | A stable code when the check failed | `row_count_changed` |
| `checks[].reason` | A short summary. It never contains table names | `Row counts moved past the allowed tolerance in 1 table...` |
| `checks[].measured` | What was measured, such as the newest timestamp in the column you chose | `2026-11-18T01:59:00Z` |
| `checks[].expected` | What the check allows | `within 10% of the last passing run` |
| `checks[].tables[].table_key` | The table that failed the check | See below |
| `checks[].tables[].problem` | `missing` or `row_count_changed` | `missing` |
| `checks[].tables[].measured_rows` | The rows found | `90` |
| `checks[].tables[].expected_rows` | The rows in the last passing run | `100` |
| `checks[].tables[].change_percent` | How far the count moved | `10` |
| `checks[].tables[].tolerance_percent` | How far it may move | `5` |
| `tables[].table_key` | A restored table | See below |
| `tables[].rows` | Its row count | `1200` |
| `notes[]` | Extra information about the run | `The backup doesn't say which Postgres version made it...` |

### Table keys

By default, a `table_key` is the lowercase hex HMAC-SHA256 of the schema, a NUL byte and the table name, keyed with the source's secret salt. Backproof can't turn it back into a name. With "Show table names" on for a source, the `table_key` is the readable `schema.table`.

### Example

```json
{
  "schema_version": 1,
  "status": "pass",
  "failed_stage": null,
  "reason_code": null,
  "reason": null,
  "backup": {
    "object_key": "prod/daily.dump.gz",
    "size_bytes": 1048576,
    "time": "2026-11-18T02:00:00Z",
    "format": "custom",
    "compression": "gzip",
    "encryption": null,
    "postgres_version": "17.4",
    "postgres_major": 17,
    "version_source": "dump",
    "restored_major": 17
  },
  "stages": [{ "name": "restore", "duration_ms": 5300 }],
  "checks": [
    {
      "name": "row_counts",
      "status": "pass",
      "code": null,
      "reason": null,
      "measured": "1 table counted",
      "expected": "within 10% of the last passing run",
      "tables": []
    }
  ],
  "tables": [
    { "table_key": "3f9a2c7d0e1b4a6f8c2d5e7a9b0c1d3e4f5a6b7c8d9e0f1a2b3c4d5e6f7a8b9c", "rows": 1200 }
  ],
  "notes": []
}
```
