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

## Jobs and results

**The runner doesn't send check-ins or job results yet.** Each field will be documented here, with an example, in the same change that starts sending it.
