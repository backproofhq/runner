# Install

The Backproof runner restores your latest Postgres backup in a throwaway container on your own machine, checks it and reports the result to Backproof. Pick the way to run it that fits your setup:

| Way | Runs | Good for |
| --- | --- | --- |
| [Agent on a server](#install-on-a-server) | All the time, asking Backproof what's due about every 30 seconds | A VPS, Dokploy, Coolify or any Linux host with Docker |
| [One-shot](#one-shot-with-cron-or-systemd) | When a timer starts it, then exits | A host where you'd rather run cron or a systemd timer |
| [GitHub Actions](#github-actions) | On a workflow schedule | Teams without a server to spare |
| [GitLab CI](#gitlab-ci) | On a pipeline schedule | GitLab users, with Docker-in-Docker |

## Requirements

- Linux on amd64 or arm64, with Docker. The install script offers to install Docker if it's missing.
- Outbound HTTPS to `api.backproof.dev`, your backup storage and Docker Hub. [security.md](security.md#network-access) lists every destination. Installing also fetches the script from `get.backproof.dev` and GitHub.
- Free disk space of about three times your largest backup. `BACKPROOF_DISK_MARGIN` in [config.md](config.md) changes the factor.

## Install on a server

Open **Add runner** in the [dashboard](https://app.backproof.dev) and copy the command. It holds a one-time install token, which works once and expires after an hour:

```sh
curl -fsSL https://get.backproof.dev | sh -s -- --token bp_inst_...
```

`get.backproof.dev` serves `install.sh` from the [latest release](https://github.com/backproofhq/runner/releases/latest), so you run the same script you can read in [`scripts/install.sh`](../scripts/install.sh). The script:

1. Checks the host and Docker, and asks before installing Docker with Docker's own script.
2. Pulls `backproofhq/runner:latest` and prints its version.
3. Registers the runner. It creates the runner's key pair and sends only the public key. The token and private key are saved in `/etc/backproof/runner.env` with mode `600`.
4. Asks to start the runner container, then waits for its first check-in. If it doesn't check in within 60 seconds, the script prints what [`backproof doctor`](#check-a-runner) finds.

It runs through `sudo` when you aren't root. Prompts read from your terminal, so they work with `curl | sh`.

| Option | What it does |
| --- | --- |
| `--token TOKEN` | The install token from the Add runner page. Only new installs need it |
| `--name NAME` | The runner's name in the dashboard. The default is the host's name |
| `--version VERSION` | Installs a specific release, such as `0.2.0-beta.1`. The default is the latest stable release |
| `--drain-timeout TIME` | How long a running backup test may take to finish when the runner stops or updates, such as `90s`, `30m` or `2h`. The default is `30m` |
| `--yes` | Accepts every prompt, for Ansible, cloud-init and other unattended installs |
| `--dry-run` | Prints every step and changes nothing |
| `--uninstall` | Removes the runner, its work volume and `/etc/backproof` |

Without a terminal and without `--yes`, the script stops before it changes anything, so the install token stays unused.

### Without the script

Register once. Docker would otherwise name the runner after the container's ID, so pass the host's name:

```sh
sudo mkdir -p -m 700 /etc/backproof
sudo docker run --rm -v /etc/backproof:/etc/backproof \
  -e BACKPROOF_NAME="$(uname -n)" \
  backproofhq/runner:latest register --token bp_inst_...
```

Then start the runner with `docker run`:

```sh
sudo docker run -d --name backproof-runner --restart unless-stopped \
  --stop-timeout 1800 \
  -v /var/run/docker.sock:/var/run/docker.sock \
  -v backproof-work:/var/lib/backproof \
  -v /etc/backproof:/etc/backproof:ro \
  backproofhq/runner:latest agent
```

Or with Docker Compose:

```yaml
services:
  backproof-runner:
    image: backproofhq/runner:latest
    command: agent
    restart: unless-stopped
    # Lets a running backup test finish when the container stops.
    stop_grace_period: 30m
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock
      - backproof-work:/var/lib/backproof
      - /etc/backproof:/etc/backproof:ro

volumes:
  backproof-work:
```

The runner reads its settings from the read-only `/etc/backproof` mount. Leave out `--env-file`, so the token doesn't end up in the container's configuration, where `docker inspect` shows it.

The runner needs the Docker socket to start its throwaway Postgres containers. Those containers have no network access and no published ports.

## Check a runner

```sh
sudo docker exec backproof-runner backproof doctor
```

`doctor` checks:

- the settings;
- that Docker can be reached;
- that Backproof accepts the token;
- that the runner's key fingerprint matches the dashboard's;
- whether a newer or required version exists;
- where each source's keys come from.

It never prints a key and never claims a job. It exits with `2` when a check fails.

## Update

Run the same command again, without a token:

```sh
curl -fsSL https://get.backproof.dev | sh
```

The dashboard shows a notice when a runner is behind, and alerts you when it's below the version Backproof still supports. The update:

1. Pulls the latest release. If the runner already runs it, the script says so and stops.
2. Asks before a new major version, which has breaking changes. `--yes` accepts. Without a terminal and without `--yes`, the runner keeps its version.
3. Stops the runner. It stops claiming jobs and finishes the running backup test, waiting up to `--drain-timeout`. A test that runs longer is stopped, and Backproof queues it again.
4. Starts the new version with the same settings. `/etc/backproof` isn't touched.
5. Waits for the new version to check in. If it doesn't, the script puts the previous version back.

An update only asks about a new major version, so a cron job can run it unattended. Beta testers install a pre-release on purpose with `--version`, such as `--version 0.2.0-beta.1`. `latest` only ever points at a stable release.

## Uninstall

```sh
curl -fsSL https://get.backproof.dev | sh -s -- --uninstall
```

This removes:

- the runner container, after its running test finishes;
- any throwaway containers and volumes it left behind;
- the `backproof-work` volume;
- `/etc/backproof`.

Delete the runner in the dashboard as well.

## One-shot with cron or systemd

`backproof run --once` runs every test that's due, then exits. It needs a registered runner, so [register](#without-the-script) first. For example, an hourly cron entry in `/etc/cron.d/backproof`:

```sh
17 * * * * root docker run --rm -v /var/run/docker.sock:/var/run/docker.sock -v backproof-work:/var/lib/backproof -v /etc/backproof:/etc/backproof:ro backproofhq/runner:latest run --once
```

The timer has to run at least as often as your most frequent backup test. Backproof flags a runner that hasn't checked in for twice its expected interval.

| Exit code | Meaning |
| --- | --- |
| `0` | Every due test passed, or nothing was due |
| `1` | At least one backup failed |
| `2` | The runner couldn't do its job, for example because Docker is down or the credentials are wrong |

A failed backup outranks a runner error.

## GitHub Actions

Create a CI token on the **Add runner** page and save it as the repository secret `BACKPROOF_TOKEN`. The CI token also holds the runner's private key, which never leaves the job. Then add `.github/workflows/backproof.yml`:

```yaml
name: Backproof
on:
  schedule:
    - cron: "17 * * * *"
  workflow_dispatch:
jobs:
  backproof:
    runs-on: ubuntu-latest
    steps:
      - uses: backproofhq/runner-action@v1
        with:
          token: ${{ secrets.BACKPROOF_TOKEN }}
```

The job fails when a backup fails or when the runner can't do its job. The [action's README](https://github.com/backproofhq/runner-action) shows how to pass local secrets instead of sealed keys.

## GitLab CI

Create a CI token on the **Add runner** page and save it as a masked CI/CD variable named `BACKPROOF_TOKEN`. Include the template in `.gitlab-ci.yml`:

```yaml
include:
  - remote: https://raw.githubusercontent.com/backproofhq/runner/main/ci/gitlab/backproof.gitlab-ci.yml
```

Then add a pipeline schedule under **Build > Pipeline schedules**. The template runs on schedules and manual pipelines, using Docker-in-Docker, so it needs a runner that allows privileged containers. GitLab.com's shared runners do. A protected variable only reaches protected branches.

## Verify a download

Each release has a `checksums.txt` that covers the archives, their SBOMs and `install.sh`. The release workflow signs it with [cosign](https://github.com/sigstore/cosign) using its GitHub identity, with no long-lived key. To verify release `v1.2.3`:

```sh
cosign verify-blob checksums.txt \
  --bundle checksums.txt.sigstore.json \
  --certificate-identity "https://github.com/backproofhq/runner/.github/workflows/release.yml@refs/tags/v1.2.3" \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
sha256sum --check --ignore-missing checksums.txt
```

The images are signed the same way:

```sh
cosign verify backproofhq/runner:1.2.3 \
  --certificate-identity "https://github.com/backproofhq/runner/.github/workflows/release.yml@refs/tags/v1.2.3" \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
```
