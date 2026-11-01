# Command reference

[Back to README](../README.md)

Every command accepts `-h` to print its full list of options.

## `mcsyncd`

Runs on the always-on machine.

| Command | Description |
| --- | --- |
| `init --data DIR` | Create the data directory, the archive key and the restic repository. Never overwrites an existing key. |
| `serve --data DIR --listen IP:PORT` | Run the service on a private address. |
| `invite --data DIR --addr IP:PORT NAME` | Create a single-use invite for a player. |
| `devices --data DIR` | List enrolled devices with ID, player, label, creation time and status. |
| `revoke --data DIR ID` | Revoke a device. Its token stops working immediately. |

Common options:

| Option | Description |
| --- | --- |
| `--data DIR` | Data directory. Default `/srv/mcsync`, or the `MCSYNC_DATA` environment variable. |
| `--restic PATH` | Path to the restic binary (`init` and `serve`). Default `restic`. |

Options of `serve`:

| Option | Description |
| --- | --- |
| `--listen IP:PORT` | Address to listen on (required). It must be an IP address of the private network, not a host name and not `0.0.0.0`. |
| `--reserve-gb N` | Free disk space to keep, in GB. Commits beyond it are refused. Default `20`. |

## `mcsync`

Runs on each player's PC.

| Command | Description |
| --- | --- |
| `enroll CODE [--label NAME]` | Enroll this PC using an invite code. Label defaults to the host name. |
| `status` | Show who you are, the official version, and who the host is or to whom the lease is reserved. |
| `run --world DIR` | Start a session: take the lease, sync the world, run the server, save periodically. |
| `handoff --world DIR` | Ask the current host to pass the lease on, then start a session like `run`. |

Common option: `--config FILE` is the configuration file. Default is `mcsync/config.json` in the user configuration directory, or the `MCSYNC_CONFIG` environment variable.

### Options of `run` and `handoff`

| Option | Description | Default |
| --- | --- | --- |
| `--world DIR` | World directory on this PC (required) | none |
| `--work DIR` | Working directory for state, mirror and recovery | `<world>.mcsync` |
| `--server-jar FILE` | Minecraft `server.jar`. Without it a simulated game runs | none |
| `--java PATH` | Java executable | `java` |
| `--jvm-args "..."` | JVM options separated by spaces, for example `"-Xmx4G"` | none |
| `--upload-every DURATION` | Interval between saves to the always-on machine | `10m` |
| `--heartbeat-every DURATION` | Interval between lease renewals | `1m` |
| `--fake-every DURATION` | How often the simulated game writes | `2s` |
| `-v` | Verbose logging | off |

Durations use Go syntax, for example `30s`, `10m`, `1h`.

Options only for `handoff`:

| Option | Description | Default |
| --- | --- | --- |
| `--handoff-timeout DURATION` | How long to wait for the host to pass the lease on before withdrawing the request | `5m` |
| `--handoff-poll DURATION` | How often to check whether the host has released | `3s` |

### How `handoff` behaves

| Situation | What happens |
| --- | --- |
| Nobody is hosting | Starts immediately, like `run`. |
| The lease is already reserved for you | Starts immediately. |
| The lease is reserved for someone else | Stops with an error; try again shortly. |
| You are already the host (maybe from another device) | Stops with an error. |
| Another request is already pending | Stops with an error; only one request can be pending. |
| Someone else is hosting | Sends the request and waits for the host to save and release. |
| Timeout or `Ctrl+C` while waiting | The request is withdrawn, and the reservation too if the host had already released. |

## Exit codes

| Code | Meaning |
| --- | --- |
| `0` | Success, or help requested |
| `1` | Runtime error |
| `2` | Invalid usage |
