# mcp-lazy

A small stdio MCP relay that starts the real server when it is needed. Recorded
initialization and listings let unused servers stay asleep. Once started, the
server stays alive by default, preserving its in-memory session state.

Version 0.3.0 is a Linux trial build. Real-client acceptance for this version is
pending. A license has not been selected yet.

Source: https://github.com/kitepon/mcp-lazy

## Build and run

Requires Go 1.27.1. TOML configuration editing uses
`github.com/pelletier/go-toml/v2` v2.4.3, pinned in `go.mod` and `go.sum`.
The relay does not require a Go runtime or contact the network at runtime.

To reproduce the distributed Linux amd64 executable, use a clean checkout of
the release commit, Go 1.27.1, and these exact options:

```sh
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -buildvcs=false -trimpath -ldflags '-s -w' -o mcp-lazy .
./mcp-lazy /absolute/path/to/node /absolute/path/to/server.mjs
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
| `--wake-command` | `MCP_LAZY_WAKE_COMMAND` | Disabled; JSON argv when set |
| `--wake-interval` | `MCP_LAZY_WAKE_INTERVAL` | `5s` |
| `--wake-timeout` | `MCP_LAZY_WAKE_TIMEOUT` | `1s` |

An explicitly supplied flag overrides its environment variable. Invalid
configuration exits with status 2 before starting the server. Startup timeout,
wake interval and wake timeout must be positive. Idle-stop accepts zero or a
positive Go duration (`500ms`, `30s`, `2m`). Boolean values use Go's flag syntax,
including `true` and `false`.

The startup deadline covers initial startup, an unanswered initialization or
pre-initialization request, and internal listing refresh after a cached
initialization is replayed. It does not set a timeout for ordinary tool calls
after startup has completed. On timeout, outstanding requests receive an error,
queued requests are discarded, and the process group is stopped. A subsequent
request can start a fresh server; previous tool calls are never retried
automatically.

On Linux and macOS, the relay creates a process group for the server. EOF,
SIGTERM, SIGINT, SIGHUP, startup failure and an unexpected server exit clean up
that group. Shutdown first closes stdin, then sends SIGTERM after 250ms and
SIGKILL after another 500ms if needed. Failure to reap the direct child within a
further second ends the relay with status 1. Descendants that explicitly detach
into another process group, including independent persistent daemons, are
outside this cleanup boundary. SIGKILL cannot be handled by the relay.

## Check and prepare the cache

The check starts the real server, so its startup work also runs. A stateful
server may register delivery owners, recover jobs or write to its data directory
even though no tool is called. Use an isolated data directory for checks when
startup must not affect live work. For Aiterm, set `AITERM_STATE_BASE` to an
isolated directory. Keep the command, working directory, protocol and any
settings that affect listings compatible with the intended registration.

```sh
mcp-lazy --check /absolute/path/to/node /absolute/path/to/server.mjs
```

The check sends `server/discover` before `initialize`, sends
`notifications/initialized`, retrieves every advertised listing, stops the
server and saves the record. It never calls a tool or runs the wake predicate.
Success prints one JSON report containing `ok`, `version`, `protocolVersion`,
`cachePath`, `beforeRequests` and per-list `count`, `pages` and `cached` values.
Failure prints its reason on stderr and exits with status 1. Configuration errors
exit with status 2. The server's stderr remains connected to stderr.

The entire protocol check shares one startup deadline. A failed check leaves the
previous cache file intact. Multi-page listings are fully counted, but are not
cached as a single page. The first real listing request then reaches the server.

The default check initialization uses protocol `2025-06-18`, empty client
capabilities and client name `mcp-lazy-check`. Supply the client's full, actual
initialization parameters when preparing its cache. For example, the captured
Claude Code 2.1.289 handshake uses:

```sh
export MCP_LAZY_CHECK_INITIALIZE='{"protocolVersion":"2025-11-25","capabilities":{"roots":{"listChanged":true},"elicitation":{"form":{},"url":{}}},"clientInfo":{"name":"claude-code","title":"Claude Code","version":"2.1.289","description":"Anthropic\u0027s agentic coding tool","websiteUrl":"https://claude.com/claude-code"}}'
```

The default discovery parameters use `_meta` with protocol `2026-07-28`, and the
`clientInfo` and `capabilities` from those initialization parameters. To preserve
a captured request's exact field order and raw JSON, supply its method and params
in `MCP_LAZY_CHECK_BEFORE`:

```sh
export MCP_LAZY_CHECK_BEFORE='[{"method":"server/discover","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientInfo":{"name":"claude-code","title":"Claude Code","version":"2.1.289","description":"Anthropic\u0027s agentic coding tool","websiteUrl":"https://claude.com/claude-code"},"io.modelcontextprotocol/clientCapabilities":{"roots":{"listChanged":true},"elicitation":{"form":{},"url":{}}}}}}]'
mcp-lazy --check /absolute/path/to/node /absolute/path/to/server.mjs
```

Shell-escaped examples use `\u0027` for the apostrophe. For exact byte matching,
load the captured JSON from a file with `MCP_LAZY_CHECK_BEFORE=$(cat before.json)`.
Only the read-only `server/discover` probe is accepted. Set
`MCP_LAZY_CHECK_BEFORE='[]'` to omit it for a server that cannot accept discovery
before initialization.

Successful discovery results are keyed by method and the full raw params hash.
Only an actual `-32601` (Method not found) response also has a fallback key that
omits `clientInfo.version`. This lets an unsupported discovery probe stay lazy
after a CLI version update, while preserving protocol, capabilities, client
identity and every other parameter. The fallback never supplies a successful
result or a different error. A later successful response clears its matching
unsupported fallback. Unknown requests still reach the real server.

If the server negotiates a different initialization revision, the check fails;
rerun with that supported revision. A check that needs interactive client input
fails explicitly. This command checks MCP startup and listings; it does not
verify hooks, approval delivery or a server's other background work.

## Wake a server for background work

Register a small condition-checking program when a server must start to recover
pending work without a tool call. The relay runs it after
`notifications/initialized` and at the configured interval while the server is
asleep. Its exit codes are:

| Exit | Meaning |
| --- | --- |
| 0 | Start the server |
| 1 | Keep sleeping |
| Any other code, launch error or timeout | Log a failure and keep sleeping |

```sh
export MCP_LAZY_WAKE_COMMAND='["/absolute/path/to/needs-recovery","--state-dir","/absolute/path/to/server-state"]'
export MCP_LAZY_WAKE_INTERVAL=5s
export MCP_LAZY_WAKE_TIMEOUT=1s
mcp-lazy /absolute/path/to/node /absolute/path/to/server.mjs
```

The JSON array is executed directly, with the relay's environment and working
directory. Shell expansion is not performed. The predicate receives closed
stdin; stdout and stderr are discarded to keep them out of the MCP stream.
Use the relay log for timeout and exit failures. Only one predicate runs at a
time, and no predicate runs while the server is alive. Normal MCP requests can
start the server while a predicate is running. Each predicate has its own process
group; timeout and relay shutdown kill it and its descendants.

When the condition matches, the relay starts the server, replays the actual
client's initialization and refreshes cached listings. With `idle-stop=0`, it
then stays alive. The wake mechanism requires a completed initialization; it
does not recover work before the client connects. The predicate must be safe to
run repeatedly. Product-specific delivery ownership and recovery remain the
predicate's responsibility; a matching condition alone does not prove delivery.

## Wrap, reapply and restore a registration

Use an explicit client file, server name and persistent state directory. The
command handles one existing stdio registration at a time. JSON files use
`--client claude` or `cursor`; TOML files use `codex` or `grok`.

```sh
mcp-lazy config wrap --client claude --file /absolute/path/to/config.json \
  --server example --state-dir /absolute/path/to/registration-state \
  --env MCP_LAZY_CACHE_DIR=/absolute/path/to/cache \
  --env MCP_LAZY_IDLE_STOP=0 --dry-run

# Run the same command without --dry-run to apply it.
mcp-lazy config wrap --client claude --file /absolute/path/to/config.json \
  --server example --state-dir /absolute/path/to/registration-state \
  --env MCP_LAZY_CACHE_DIR=/absolute/path/to/cache --env MCP_LAZY_IDLE_STOP=0

# Run after the original product's setup/register has finished.
mcp-lazy config reapply --client claude --file /absolute/path/to/config.json \
  --server example --state-dir /absolute/path/to/registration-state

mcp-lazy config status --client claude --file /absolute/path/to/config.json \
  --server example --state-dir /absolute/path/to/registration-state
mcp-lazy config unwrap --client claude --file /absolute/path/to/config.json \
  --server example --state-dir /absolute/path/to/registration-state
```

The relay path defaults to this executable. Use `--relay /absolute/path/to/new`
to select or upgrade it. Each `--env MCP_LAZY_NAME=value` sets a managed relay
option. Reapply is idempotent and never adds a second wrapper. If setup restored a
direct registration, reapply adopts its latest command and arguments before
wrapping it. Other client settings, including `env_vars`, timeouts, permissions,
working directory and unrelated registrations, are kept. Unwrap restores the
latest direct command and arguments, and restores only environment values that
still match the managed overrides. Later user changes to those values are kept.
An empty environment object/table may remain after its managed keys are removed.

`--dry-run` validates the edited document and reports changed field names without
writing files or printing values. Changed configuration files are backed up in
`<state-dir>/backups/`; backups and registration metadata have mode 0600. They may
contain credentials, so keep the state directory private. The edited config
keeps its existing permissions. JSON formatting is normalized, with unknown
settings and large integer values preserved. TOML expressions are parsed with
the library; unedited settings and comments remain unchanged. A TOML registration
must use ordinary or dotted table keys; an enclosing inline-table registration
is rejected without writing the configuration. Inline `env` tables are supported.
HTTP registrations and unmanaged nested relays are rejected.

Use one state directory for all relay edits to a configuration file. Relay
commands serialize those edits on Linux/macOS. External setup/register commands
do not share that lock: run reapply after they finish, and avoid simultaneous
external edits. Changes noticed before replacement are rejected and the backup
is retained. This command does not monitor configuration files or restart an
already connected CLI. Reconnect the client to use the new registration.

## Trial limitations

- Linux amd64 has been tested. macOS process-group and configuration-lock code is
  provided but has not passed real-machine acceptance. Other platforms are
  untested, only stop the direct process, and do not support config writes.
- Aiterm must use `idle-stop=0`. Full deployment remains gated on delivery tests
  and a verified wake/recovery predicate. Approval Box's independent daemon must
  continue to be started and recovered by its existing setup.
- Cached initialization entries are still partitioned by protocol version;
  the file key uses the command and file metadata. Client-dependent or
  environment-dependent listings need separate cache directories until richer
  cache scoping is implemented. A check is only a suitable warmup when its
  initialization and server configuration are compatible with the live client.
- The initialize-free protocol does not yet get lazy cached listings. Recorded
  discovery responses support a client's fallback to the initialize handshake.

## Verification

```sh
go test -v -timeout 90s ./...
go vet ./...
MCP_LAZY_TEST_RACE=1 GORACE=atexit_sleep_ms=0 go test -race -timeout 90s ./...
```

The race command instruments both the test helper and the relay binary exercised
by subprocess tests. Tests cover cached and cold startup, failed-request retry,
blocked stdin, all listings and metadata forwarding; checked discovery and CLI
version changes; SIGHUP cleanup; wake conditions, deadlines and ordinary calls
during a predicate; and JSON/TOML wrapping, repeated application, setup changes,
restoration, backups and preservation of unrelated settings.

With Node.js and the real server executables available, an optional isolated
protocol smoke test compares discovery, initialization and listings with a
direct connection, then checks first-session lazy startup, wake startup and
SIGHUP cleanup:

```sh
node tools/real-stdio-smoke.mjs /absolute/path/to/mcp-lazy \
  /absolute/path/to/smoke-artifacts /absolute/path/to/approval-box/cli.mjs \
  /absolute/path/to/aiterm-mcp/index.js
```

It uses isolated HOME and Aiterm state, points Approval Box at an unreachable
local endpoint and invokes no tools. It does not test an AI CLI or actual
approval/child-answer delivery.
