# Limitations and roadmap

[Back to README](../README.md)

mcsync is in **alpha**. The core is complete and tested, but it should not yet be the only copy of a world you care about.

## Validation status

- The full workflow has been tested extensively with a simulated game server, including real HTTP between a real service and real agents.
- A real Minecraft server has been used for individual steps. A complete 10-minute cycle on a large modded world, with several players connected, has not been run yet.
- Repeated stress runs and the race detector have been used on Linux only. The regular test suite passes on Windows as well.
- In a home network, Tailscale gives a direct connection (about 12 to 13 ms). Relayed connections and real bandwidth between different households have not been measured.

## Known limitations

- **Offline mode.** If the always-on machine is unreachable at startup, no session can start. While playing, the host stops after 8 minutes without it.
- **Player transfer during a handoff.** Players connected to the old host are not moved automatically; they must reconnect to the new host.
- **Late handoff request.** If a request arrives after the host's last heartbeat and before it closes, the host releases with no reservation. The requester takes the lease as soon as it sees it free, but a third player could be faster. No data is lost.
- **Interrupting a startup download.** Pressing `Ctrl+C` while the world is still downloading ends the session with a technical message ("download… context canceled"). Nothing is saved and the world on the always-on machine is not affected.
- **External archive engines.** If an engine other than the built-in one is used, there is not yet a defined way to stop clients from deleting versions.
- **Not verified:** world layouts of old Minecraft versions and of Paper servers, `*_old` files, behaviour of a paused server after 60 seconds, and Java versions needed by old mod loaders.
- **Launcher hooks.** Prism Launcher's pre-launch hook can block a launch, but its post-exit hook does not run after a forced close, so mcsync does not rely on launcher hooks.

## Roadmap

- [ ] Offline mode for when the always-on machine is unreachable
- [ ] Clearer message when interrupting a startup download
- [ ] Local web interface
- [ ] Telegram notifications and a status page
- [ ] Archive backup to external storage
- [ ] Option to run without a dedicated always-on machine, using remote storage
