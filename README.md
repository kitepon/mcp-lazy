# mcp-lazy

A small stdio MCP relay that starts the real server when it is needed. Recorded
initialization and listings let unused servers stay asleep. Once started, the
server stays alive by default, preserving its in-memory session state.

Version 0.2.0 is a Linux trial build. Real-client acceptance is pending. The
public name and license have not been selected yet.

## Build and run

Requires Go 1.27.1. There are no third-party Go dependencies.

```sh
go build -trimpath -o mcp-lazy .
mcp-lazy /absolute/path/to/node /absolute/path/to/server.mjs
```

Put the relay in the existing MCP registration's `command`, and put the original
command before the original arguments in `args`. Keep the server's registration
name, environment, working directory and other client settings.

```json
{
  "command": "/absolute/path/to/mcp-lazy",
  "args": ["/absolute/path/to/node", "/absolute/path/to/server.mjs"],
  "env": {
    "MCP_LAZY_START_TIMEOUT": "60s",
    "MCP_LAZY_IDLE_STOP": "0",
    "MCP_LAZY_CACHE_DIR": "/absolute/path/to/cache"
  }
}
```

Options and `--` are optional. This allows registrations whose existing doctor
checks the first argument as a file. Environment settings work without adding
relay options to `args`.

## Configuration

| Flag | Environment variable | Default |
| --- | --- | --- |
| `--start-timeout` | `MCP_LAZY_START_TIMEOUT` | `60s` |
| `--idle-stop` | `MCP_LAZY_IDLE_STOP` | `0` (keep alive) |
| `--cache-dir` | `MCP_LAZY_CACHE_DIR` | User cache directory + `/mcp-lazy` |
| `--log-file` | `MCP_LAZY_LOG_FILE` | No file |
| `--verbose` | `MCP_LAZY_VERBOSE` | `false` |

An explicitly supplied flag overrides its environment variable. Invalid
configuration exits with status 2 before starting the server. The startup timeout
must be positive. Idle-stop accepts zero or a positive Go duration (`500ms`, `30s`,
`2m`). Boolean values use Go's flag syntax, including `true` and `false`.

The startup deadline covers initial startup, an unanswered initialization or
pre-initialization request, and internal listing refresh after a cached
initialization is replayed. It does not set a timeout for ordinary tool calls
after startup has completed. On timeout, outstanding requests receive an error,
queued requests are discarded, and the process group is stopped. A subsequent
request can start a fresh server; previous tool calls are never retried
automatically.

On Linux and macOS, the relay creates a process group for the server. EOF,
SIGTERM, SIGINT, startup failure and an unexpected server exit clean up that
group. Shutdown first closes stdin, then sends SIGTERM after 250ms and SIGKILL
after another 500ms if needed. Failure to reap the direct child within a further
second ends the relay with status 1, rather than starting another generation.
Descendants that explicitly detach into another process group, including
independent persistent daemons, are outside this cleanup boundary.

## Check and prepare the cache

Run the check in the same working directory and with the same server environment
as the intended registration:

```sh
mcp-lazy --check /absolute/path/to/node /absolute/path/to/server.mjs
```

The check always starts the real server. It initializes it, sends
`notifications/initialized`, retrieves every advertised listing, saves the
record and stops the server. It never calls a tool. Success prints one JSON
report on stdout containing `ok`, `version`, `protocolVersion`, `cachePath` and
per-list `count`, `pages` and `cached` values. Failure prints its reason on stderr
and exits with status 1. Configuration errors exit with status 2. The server's
stderr remains connected to stderr.

The entire protocol check shares one startup deadline. A failed check leaves the
previous cache file intact. Multi-page listings are fully counted, but are not
cached as a single page. The first real listing request then reaches the server.

The default check initialization uses protocol `2025-06-18`, empty client
capabilities and client name `mcp-lazy-check`. To use a client's real identity or
another supported handshake revision, supply the full initialization parameters:

```sh
export MCP_LAZY_CHECK_INITIALIZE='{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"Claude Code","version":"1"}}'
mcp-lazy --check /absolute/path/to/node /absolute/path/to/server.mjs
```

If the server negotiates a different revision, the check fails and reports it;
rerun with that supported revision. A check that needs interactive client input
fails explicitly. This command checks MCP startup and listings; it does not
verify hooks, approval delivery or a server's other background work.

## Trial limitations

- Linux amd64 has been tested. macOS process-group code is provided but has not
  passed real-machine acceptance. Other platforms are untested and only stop
  the direct process.
- An unused server may still need to receive background events or recover
  deliveries. A generic wake condition is planned for the next version. Do not
  enable lazy startup for such servers without a separate recovery path.
- Aiterm must use `idle-stop=0`. Full deployment remains gated on delivery tests
  and a wake/recovery policy. Approval Box's independent daemon must continue to
  be started and recovered by its existing setup.
- Existing `setup`/`register` commands can overwrite the relay registration.
  The host's registration process must reapply it afterward. This version does
  not edit or watch client configuration files itself.
- Cached initialization entries are still partitioned by protocol version;
  the file key uses the command and file metadata. Client-dependent or
  environment-dependent listings need separate cache directories until richer
  cache scoping is implemented. A check is only a suitable warmup when its
  initialization and server configuration are compatible with the live client.
- The initialize-free protocol does not yet get lazy cached listings. Existing
  pre-initialization request recording is retained.

## Verification

```sh
go test -v -timeout 90s ./...
go vet ./...
MCP_LAZY_TEST_RACE=1 GORACE=atexit_sleep_ms=0 go test -race -timeout 90s ./...
```

The race command instruments both the test helper and the relay binary exercised
by subprocess tests. Tests cover cold and cached initialization timeouts, retry
without replaying failed calls, blocked stdin, configuration precedence, process
group cleanup, cache preservation on failed checks, pagination, all advertised
list types, and result/environment/metadata forwarding.
