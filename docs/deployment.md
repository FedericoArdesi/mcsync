# Deployment

[Back to README](../README.md)

This guide sets up one always-on machine and any number of player PCs.

## Before you start

- A private network that connects every player's PC to the always-on machine, for example [Tailscale](https://tailscale.com). mcsync does not require any port to be opened on your router.
- On the always-on machine: Linux, [restic](https://restic.net) in `PATH`, and enough disk space for the world and its history.
- On player PCs: Windows or Linux, plus Java and a Minecraft `server.jar` for real play.

Build the two programs as described in the [README](../README.md#installation). Copy `mcsyncd` to the always-on machine and `mcsync` to each player's PC.

## 1. The always-on machine

Initialize the data directory and the archive:

```sh
mcsyncd init --data /srv/mcsync
```

This creates the data directory, a restic repository and the repository key at `/srv/mcsync/state/repo.key`.

> **Back up the key now.** Without it, saved worlds cannot be read. Keep a copy outside the machine, for example in a password manager. Do not store it inside the repository or on a shared service.

Start the service on the machine's private address:

```sh
mcsyncd serve --data /srv/mcsync --listen 100.x.y.z:8765
```

The service refuses to listen on all interfaces or on a host name. `--reserve-gb` (default 20) is the free disk space it keeps for the system; commits that would go below it are refused.

### Running as a service (systemd)

An example unit file, to adapt to your paths, user and address:

```ini
# /etc/systemd/system/mcsyncd.service
[Unit]
Description=mcsync service
After=network-online.target
Wants=network-online.target

[Service]
ExecStart=/usr/local/bin/mcsyncd serve --data /srv/mcsync --listen 100.x.y.z:8765
Restart=on-failure
User=mcsync

[Install]
WantedBy=multi-user.target
```

```sh
sudo systemctl daemon-reload
sudo systemctl enable --now mcsyncd
```

The user must be able to read and write the data directory. If the private-network address only exists after your VPN is up, make the unit start after the VPN service.

## 2. Invite the players

Create one invite per player:

```sh
mcsyncd invite --data /srv/mcsync --addr 100.x.y.z:8765 player1
```

`--addr` is the address players will use to reach the service. Player names are 3 to 16 characters: letters, digits and underscore. An invite code is single-use and expires after 24 hours; send it only to the intended player.

Manage devices at any time:

```sh
mcsyncd devices --data /srv/mcsync        # list enrolled devices
mcsyncd revoke  --data /srv/mcsync ID     # revoke one device
```

While the service is running on Linux, these commands talk to it through a local socket. On Windows, invites and revocations can only be made while the service is stopped.

## 3. Each player's PC

Enroll once, using the invite code:

```sh
mcsync enroll <invite-code> --label "Player 1 PC"
mcsync status
```

`status` shows who the current host is and the latest official version. The device token is saved in the user configuration directory (`mcsync/config.json`) with owner-only permissions. To enroll again, delete that file and use a new invite.

### Your first session

```sh
mcsync run --world /path/to/world \
           --server-jar /path/to/server.jar \
           --java /path/to/java \
           --jvm-args "-Xmx4G"
```

- On the very first launch, the Minecraft server stops because `eula.txt` contains `eula=false`. Open the file in the server directory and set `eula=true`.
- The world directory is the only part of the server that is synchronized. `server.jar`, `eula.txt`, `server.properties` and the whitelist stay in the server directory, next to the jar.
- mcsync passes the world location to the server itself, so `server.properties` does not need to be edited for it.
- Running the server with `online-mode=true` is recommended.
- If no official version exists yet, the first session starts from the local world and its first save becomes version 1.

Without `--server-jar`, `mcsync` runs a simulated game. It is useful to check that enrollment, saving and handoff work before involving Minecraft.

## Data layout

On the always-on machine, everything lives under `--data`:

```
repo/     restic repository (world versions)
state/    lease state, device list, archive key, local admin socket
work/     transfers in progress
```

On a player's PC:

- the **world** in the directory given with `--world`;
- a **working directory** (by default `<world>.mcsync`, next to the world) holding local state, the mirror used while saving, and recovery copies;
- the **configuration file** with the service address and the device token.

## Backups

The archive on the always-on machine is the only official copy of the world. Back up both `repo/` and `state/repo.key`, and keep them in different places. Built-in backup to external storage is on the [roadmap](limitations.md#roadmap).
