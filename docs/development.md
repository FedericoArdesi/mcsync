# Development

[Back to README](../README.md)

## Building and testing

```sh
go build ./cmd/mcsync ./cmd/mcsyncd
go vet ./...
go test ./...
go test -race ./...     # Linux
```

Requires Go 1.22 or later. There are no external Go dependencies. The CI workflow runs `go vet` and `go test` on Linux and Windows for every push to `main` and every pull request.

The tests exercise the protocol, the service, the agent, the HTTP API and the commands, including end-to-end tests with a real service, real HTTP and real agents driving a simulated game.

## Trying the whole workflow without Minecraft

`mcsync run --world DIR` without `--server-jar` starts a simulated game that writes into the world at a regular interval (`--fake-every`). With a service running on a private address you can enroll, play, save, hand off and recover without any Minecraft files.

## Code layout

```
cmd/
  mcsyncd/        service entry point
  mcsync/         player agent entry point
internal/
  protocol/       coordinator: lease, generations, commits, handoff (no networking)
  statefile/      coordinator state on disk, written atomically
  service/        connects the coordinator to the archive
  server/         HTTP server and handlers
  api/            paths, error codes and messages shared with clients
  client/         HTTP client used by the agent and commands
  agent/          session lifecycle, Minecraft process, simulated game, final save
  archive/        archive interface, restic backend, in-memory store for tests
  auth/           devices, invites and tokens
  admin/          local administration while the service runs
  tarutil/        packing and unpacking of worlds
  atomicfile/     atomic file writes
  testutil/       shared test helpers
```

The protocol logic in `internal/protocol` has no I/O and takes a clock as a dependency, which keeps it fully testable. The API definitions in `internal/api` are shared by server and client, so the two sides cannot drift apart.

## Conventions

- Code comments and user-facing messages are currently in Italian, and some comments refer to sections of a design document that is not part of this repository.
- Errors the user must act on carry a clear message and a stable error code; the code is for machines and does not change, the message may.
