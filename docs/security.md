# Security and trust model

Backproof tests whether your database backups restore. To do that, the runner needs your backups and the keys to open them. This document explains what we promise, how the runner keeps those promises, and where the limits are.

If you find a security issue, see [SECURITY.md](../SECURITY.md) for how to report it.

## Our promises

1. **Your data stays in your infrastructure.** Backups are downloaded, decrypted and restored only on the machine or CI job where you run the runner. Dumps, row contents and SQL never leave it.
2. **Only metadata leaves.** The runner sends us results such as pass or fail, row counts, timings and sizes. Every field is listed in [payload.md](payload.md).
3. **We can't read your keys.** Keys you enter in the dashboard are encrypted in your browser for your own runners before they reach us. Our servers store only ciphertext we can't open.
4. **The runner never touches production.** It needs a read-only key to your backup bucket. It never asks for database credentials and never connects to your databases.
5. **No inbound connections.** The runner only makes outbound HTTPS calls. It opens no ports.
6. **You can check all of this.** The runner and the sealing code are open source under Apache 2.0, and every release is signed.

## What leaves your infrastructure

The full list, with an example, is in [payload.md](payload.md). In short:

| Sent to Backproof | Never sent |
| --- | --- |
| Result: pass, fail or error | Backup files or any part of them |
| The stage that failed, with a reason code and safe details such as an extension name or version number | Row contents or query results |
| Time per stage | SQL from the dump, or raw `pg_restore` and `psql` output |
| Backup object key and size | Your bucket keys or decryption keys in readable form |
| Detected Postgres version | Database credentials (the runner never has them) |
| Row counts per table, with table names hashed unless you turn on plain names | |
| Each check's measured and expected value, such as the newest timestamp in the column you chose | |
| Runner name, version, OS and architecture | |

Raw output from the restore tools stays in the runner's local logs on your machine, because it can contain table names or pieces of SQL.

### Table names

Table names are hashed by default. The runner sends an HMAC-SHA256 of each name, keyed with a random salt for that backup source. The salt is created in your browser and sealed with your other keys, so we can't recover names by hashing common ones like `users`. If you want readable names in the dashboard, turn on "Show table names" for that source.

## How your keys are handled

The runner needs two kinds of secrets: a read-only key for your bucket, and, if your backups are encrypted, the age or GPG key to decrypt them. You choose one of two modes per source.

### Sealed in the dashboard (default)

1. When a runner is installed, it creates its own age X25519 key pair. The private key stays on the runner; only the public key is sent to us.
2. When you enter keys in the dashboard, your browser encrypts them with the public keys of your runners. The plain keys never leave your browser.
3. We store the encrypted bundle and send it to your runners with each job.
4. The runner decrypts the bundle in memory for that one job. It never writes the keys to disk or logs them.

A new runner can't open bundles sealed before it existed, so the dashboard asks you to enter the keys again for it.

For CI runners, the private key is part of the runner token you store as a CI secret. Treat that token like any other secret.

### Local secrets

If you want no secret to pass through Backproof at all, put the keys in `/etc/backproof/sources.env` on the runner, or in your CI secrets. Local secrets take precedence over sealed ones. See [config.md](config.md) for the variable names.

### What sealing protects against

- **A leak of our database, or anyone at Backproof reading it.** Sealed bundles can't be opened without your runner's private key.
- **Other customers.** A bundle opens only with the private key of a runner it was sealed for. Reading the open-source code doesn't change that.
- **Swapped runner keys.** Each runner's key fingerprint is shown by the install script, by `backproof doctor` and in the dashboard, so you can compare them. The dashboard warns loudly and stops sealing if a runner's key changes after registration.

### What sealing can't fully protect against

Sealing happens in the dashboard's JavaScript, which we serve. If our dashboard were compromised, it could in theory capture keys while you type them. We reduce this risk with a strict Content Security Policy and no third-party scripts on pages where keys are entered. If your threat model doesn't allow trusting our served code, use local secrets.

## Permissions you give

- **Bucket key for the runner:** read and list only, ideally limited to the bucket and prefix that hold your backups.
- **Bucket key for the monitor:** if you use the free monitor, it gets a separate list-only key so it can check backup freshness and size. Keep it separate from the runner's key, so the key Backproof holds can never download a backup.
- **No database credentials,** ever.

## The runner host

- **Docker socket.** The runner starts throwaway Postgres containers through the Docker socket, which gives it root-level control of Docker on that host. Run it on a dedicated VM or server, or use the GitHub Action or GitLab CI job, rather than on a production machine.
- **Throwaway containers.** Restores run in official `postgres` images with no network and no published ports. Each container is removed after its job, also after a failure or timeout. Leftovers from a crash are removed when the runner next starts.
- **Decrypted dumps on disk.** During a job, the decrypted dump sits in the runner's work volume and is deleted when the job ends. We recommend disk encryption on the runner host.
- **Minimal image.** The runner image is built on a distroless base: no shell or package manager.

## Network access

The runner connects out to:

| Destination | Why |
| --- | --- |
| `app.backproof.dev` (HTTPS) | Jobs, heartbeats and results |
| Your storage endpoint | Downloading backups |
| Docker Hub | Pulling the runner image and `postgres` images |

If you use a firewall allowlist, these are all it needs. The restored database itself has no network access.

## Verifying what you run

- **Source:** everything is at [github.com/backproofhq/runner](https://github.com/backproofhq/runner). You can build the binary and image yourself.
- **Signatures:** release images and checksums are signed with cosign, and each release ships an SBOM.
- **Install script:** `get.backproof.dev` serves `scripts/install.sh` from the latest stable release. Run it with `--dry-run` to see every step without changing anything.

## Our side

- Runner tokens are stored only as hashes. Install tokens work once and expire after one hour.
- Every API route has automated tests that refuse access with another team's credentials.
- Results are kept only for your plan's retention period, then deleted.
- The Backproof API and dashboard are hosted in the EU.
- The API, authentication and sealing flow get an independent security review before runner 1.0.

## Not covered

- **A compromised runner host.** Whoever controls that machine can see what the runner sees during a job. Protect it like any server that handles backups.
- **Your keys elsewhere.** We can't protect copies of your bucket or decryption keys kept outside Backproof.
- **Your backups themselves.** We test whether they restore; we don't store, encrypt or secure them.