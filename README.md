# DomainChecker

[![Go](https://github.com/Daedelus1/domainchecker/actions/workflows/go.yml/badge.svg)](https://github.com/Daedelus1/domainchecker/actions/workflows/go.yml)

A [Squid](http://www.squid-cache.org/) `external_acl_type` helper that puts a
human in the loop for web requests. Squid asks the helper "allow or deny
this URL?"; the helper relays that question over a WebSocket to whoever is
connected, waits for a `y`/`n` answer, and tells Squid the verdict.

## How it works

```
Squid  --stdin-->  Server  <--WebSocket-->  Client  --stdin-->  Human
  ^                                                                |
  '-------------------------- stdout (OK/ERR/BH) -------------------'
```

1. Squid spawns the server as an `external_acl_type` helper and feeds it one
   line per request on stdin (`<client-ip> <encoded-url> ...`), per its
   [helper protocol](https://wiki.squid-cache.org/Features/AddonHelpers).
2. The server forwards the URL to a connected WebSocket client and blocks
   waiting for an answer.
3. The client is a small terminal program: it shows `Allow "https://..."? (y/n)`
   and reads the answer from whoever's at the keyboard.
4. The answer travels back over the WebSocket, and the server writes `OK`
   or `ERR` to stdout for that line, exactly as Squid expects.

If no one answers in time, or no client is connected at all, the request is
denied by default. If a client disconnects mid-request, the server retries
against the next client that connects.

## Features

- **Human-in-the-loop ACLs** — approve or deny proxy requests interactively
  instead of relying on a static blocklist.
- **Multiple concurrent clients** — any number of terminal clients can be
  connected; requests are dispatched to whichever one is currently
  reachable.
- **Liveness detection in both directions** — the server heartbeats each
  client and gives up on one that goes silent; the client does the same for
  the server, so a wedged TCP connection doesn't hang forever.
- **Safe retries** — a request that times out waiting for an answer is
  retried against another client, with each retry tagged so a late answer
  to a superseded attempt can't resolve the wrong outcome.
- **Rememberable decisions** — a human can answer once (`y`/`n`), or for
  every future request to that exact URL (`yu`/`nu`) or that whole domain
  (`yd`/`nd`), skipping the prompt on a repeat visit. A denial always wins
  over a contradicting remembered allow, at any scope. Remembered
  decisions are saved to disk as JSON and reloaded on startup, so they
  survive a client restart.
- **Denial reasons** — every `ERR` carries a human-readable explanation
  (timed out, no client connected, denied once, or denied and remembered
  at a given scope), which Squid can surface to the blocked user — see
  [Surfacing denial reasons](#surfacing-denial-reasons) below.

## Project layout

```
client/                   DomainCheckerClient: connects to the server,
                           prompts a human, and reports the answer back.
client/main/               Process entry point for the client binary.
server/main/               Process entry point for the server binary:
                           starts the HTTP/WebSocket listener and the
                           stdin helper loop.
server/domainchecker/      Squid helper-protocol glue: reads stdin lines,
                           writes OK/ERR/BH to stdout.
server/clientmanager/      Tracks connected clients, heartbeats, and
                           in-flight request/response matching.
shared/                    Wire message types, JSON (de)serialization, and
                           the DenyMessage* constants shown on ERR.
shared/websocketclient/    Shared WebSocket read/write loop used by both
                           the server (per connected client) and the client.
shared/stdinhandler/       Reads human y/n answers from an injectable
                           io.Reader without blocking message handling.
squid/                     A runnable example squid.conf, wired per
                           Wiring into Squid and Surfacing denial reasons
                           below, plus the custom error page it points at.
```

`flake.nix`/`flake.lock` package both binaries with Nix — see
[Building → With Nix](#with-nix).

## Requirements

- Go 1.26 or later, **or**
- Nix with flakes enabled (for the commands under [With Nix](#with-nix))

## Building

```sh
go build -o domainchecker-server ./server/main
go build -o domainchecker-client ./client/main
```

### With Nix

A flake is included for hermetic, reproducible builds — useful for baking
a binary into a container image without needing a Go toolchain in that
image at all (see
[Deploying alongside Squid in a container](#deploying-alongside-squid-in-a-container)):

```sh
nix build .#domainchecker-server   # -> ./result/bin/domainchecker-server
nix build .#domainchecker-client   # -> ./result/bin/domainchecker-client
nix run .#domainchecker-server     # build + run in one step
```

Both are the same plain, statically-linked (`CGO_ENABLED=0`) binaries the
`go build` commands above produce. `packages.<system>.default` is
`domainchecker-server`. Other flakes can depend on this one directly —
`domainchecker.packages.<system>.domainchecker-server` — to pull in the
built binary as an input rather than building it themselves.

## Testing

```sh
go test ./...                 # full suite
go test ./... -short          # skip the couple of genuinely slow tests
go test ./... -race           # recommended -- this codebase is concurrency-heavy
```

A few tests exercise real wall-clock timeouts (e.g. the ~10-12s client/
heartbeat liveness checks) and are guarded behind `-short` so the everyday
edit-run loop stays fast, while still running as part of the default
`go test ./...`.

Most packages test only through their public API (`package foo_test`).
`server/clientmanager`'s `RequestRegistry` tests and `server/domainchecker`'s
tests are the exception, staying internal (`package clientmanager` /
`package domainchecker`): they substitute an in-memory fake for an
intentionally unexported interface (`clientDispatcher` / `requestMaker`),
which is what lets the retry/attempt/failover logic be tested
deterministically -- via `testing/synctest`'s fake clock -- without a real
network connection.

## Running

Start the server. It listens for WebSocket connections on `:8080/ws` and
reads Squid-protocol lines from stdin:

```sh
./domainchecker-server
```

In another terminal (or on another machine with access to port 8080),
start a client and leave it running to answer prompts as they arrive:

```sh
./domainchecker-client
```

### Wiring into Squid

A complete, runnable example combining this section with
[Surfacing denial reasons](#surfacing-denial-reasons) below is in
[`squid/squid.conf`](squid/squid.conf).

```squid.conf
# children-max=1 / children-startup=1: this helper binds a fixed TCP port
# (:8080) for its WebSocket listener and reads one serial stdin stream, so
# more than one running instance would collide on that port. Squid's
# external_acl_type default is children-max=5, which would try to start
# exactly that many.
external_acl_type domain_checker children-max=1 children-startup=1 \
    ttl=3600 negative_ttl=10 \
    %SRC %URI /path/to/domainchecker-server

acl ask_human external domain_checker
http_access allow ask_human
```

Squid will launch the server as a subprocess, feeding it classification
requests on stdin while the server's WebSocket listener stays open for
clients to connect over the network.

- `ttl=3600` is Squid's own default: an _approved_ URL is cached for an
  hour before the helper is asked again, so a human isn't re-prompted for
  every sub-resource of a page they already allowed.
- `negative_ttl=10` is set well below Squid's default (which otherwise
  matches `ttl=`): a _denied_ verdict is only remembered for 10 seconds, so
  an accidental "n" — or a timeout because no one was at the keyboard —
  doesn't lock a site out for a full hour. Tune both to taste.

#### Surfacing denial reasons

Every `ERR` the server writes to stdout carries _why_ the request was
denied, via Squid's `message=` keyword:

```
ERR message="Denied (remembered for this domain)"
```

The possible values (all defined as constants in `shared/deny_messages.go`,
so client and server can't drift out of sync on the wording) are:

| Message                               | When                                                                                                |
| ------------------------------------- | --------------------------------------------------------------------------------------------------- |
| `Denied (not remembered)`             | A one-off `n` answer, or an unrecognized reply treated as a fail-safe denial.                       |
| `Denied (remembered for this URL)`    | An `nu` answer — denied, and remembered for that exact URL.                                         |
| `Denied (remembered for this domain)` | An `nd` answer — denied, and remembered for that whole domain.                                      |
| `Denied (previously remembered)`      | The URL or its domain was already remembered as denied from an earlier answer; no prompt was shown. |
| `Denied (no response from client)`    | No answer arrived within `USER_INPUT_TIMEOUT_DURATION`.                                             |
| `Denied (no client connected)`        | No client was connected at all when the request arrived.                                            |

A message is quoted per the helper protocol — wrapped in double quotes,
with embedded `\` and `"` backslash-escaped and any `\n`/`\r` collapsed to
a space, so it can never desync Squid's line-based stdin/stdout framing
even if the wording above changes to something with special characters in
it (`server/domainchecker/domainchecker.go`'s `quoteSquidValue`, exercised
directly by `TestHandleStdioDeniedWithMessage`). On success, no
`message=` is emitted at all — the line is a bare `OK`.

Squid exposes `message=`'s value to error page templates as the `%o`
format tag, but the stock `ERR_ACCESS_DENIED` template it ships with
doesn't reference that tag, so by default the reason goes nowhere but
Squid's own cache log. To actually show it to the person being blocked,
point the ACL at a custom copy of the error template with `%o` added to
it:

```squid.conf
deny_info ERR_CUSTOM_ACCESS_DENIED ask_human
```

1. Copy the stock template (e.g. `/usr/share/squid/errors/en/ERR_ACCESS_DENIED`)
   into Squid's `error_directory` as `ERR_CUSTOM_ACCESS_DENIED`, adding `%o`
   somewhere in its body — or just use
   [`squid/ERR_CUSTOM_ACCESS_DENIED.html`](./squid/ERR_CUSTOM_ACCESS_DENIED.html)
   in this repo, which already does.
2. Add the `deny_info` line above, referencing the same ACL name used in
   `http_access` (`ask_human` in the example under
   [Wiring into Squid](#wiring-into-squid)).
3. Reload Squid's config (`squid -k reconfigure`).

With that in place, a denied request shows the specific reason — e.g.
"Denied (remembered for this domain)" — directly on the block page,
instead of a generic "Access Denied".

#### Exempting Claude Code's own infrastructure

> **TODO (end user):** if you're running this in front of Claude Code (or
> any agent whose own plumbing needs to stay reachable regardless of
> human review), read this section and confirm
> `server/clientmanager/request-registry.go`'s `allowedInfraDomains` set
> matches your actual auth method before relying on it. It currently
> exempts only `api.anthropic.com`. **If a domain you need seems to be
> missing, please open an issue instead of editing the code directly** —
> this is a security-relevant allowlist, and changes to it should go
> through review rather than a silent local edit.

Every request going through `ask_human` includes Claude Code's own API
calls, auth, and telemetry traffic — not just the URLs the agent fetches
as part of its actual work. Prompting a human for *that* traffic risks
the agent failing to even boot if no one's watching the client at the
exact right moment, so it's reasonable to exempt it — but only it. This
exemption is implemented in domainchecker itself, not in Squid: a URL
whose host is in `allowedInfraDomains`
(`server/clientmanager/request-registry.go`) is approved by
`RequestRegistry.MakeRequest` immediately, before it ever reaches a
human or a client — deliberately kept as one source of truth instead of
also maintaining a parallel Squid ACL that could drift out of sync.

Claude Code's authoritative list of hosts it needs
(<https://code.claude.com/docs/en/network-config>) breaks down as:

- **Core plumbing** — `api.anthropic.com` (all API requests, feature
  flags, telemetry) always; `claude.ai`/`claude.com`/`platform.claude.com`/`sso.claude.com`
  only if this instance logs in interactively via OAuth/claude.ai rather
  than a static `ANTHROPIC_API_KEY`.
- **Feature-gated** (`mcp-proxy.anthropic.com`, `downloads.claude.ai`,
  `code.claude.com`, etc.) — only matters if that specific feature is in
  use; mostly irrelevant for a headless sandboxed agent.
- **Pure telemetry** (the two Datadog intake hosts) — disable via
  `DISABLE_TELEMETRY=1` instead of exempting; one env var beats adding
  hosts to a permanent bypass list.

**Do not add `registry.npmjs.org`, `github.com`, or anything else the
agent reaches to actually do its work.** Those overlap exactly with the
traffic this whole proxy exists to put a human in front of — an agent
running `npm install` or cloning a repo is doing its job, not booting
its own infrastructure, and exempting that defeats the point of
deploying this at all. `allowedInfraDomains` is pre-populated with only
`api.anthropic.com`, with the OAuth-login hosts present but commented
out — confirm which your deployment actually needs rather than enabling
both defensively.

## Configuration

There's no config file; the relevant values are constants in source. The
most likely ones to tune:

| Constant                      | Default                  | Where                                                                                                                       |
| ----------------------------- | ------------------------ | --------------------------------------------------------------------------------------------------------------------------- |
| `USER_INPUT_TIMEOUT_DURATION` | 10s                      | `client/client.go` — how long a human has to answer one prompt                                                              |
| `REQUEST_TIMEOUT_DURATION`    | 12s                      | `server/clientmanager/request-registry.go` — how long the server waits for an answer before retrying against another client |
| `TIMEOUT_DURATION`            | 10s                      | `shared/websocketclient/client.go` — how long a connection can go silent before being treated as dead                       |
| `HEARTBEAT_INTERVAL`          | 3s                       | `server/clientmanager/client-manager.go` — how often the server pings each client                                           |
| `SERVER_URI`                  | `ws://localhost:8080/ws` | `client/main/main.go` — where the client connects                                                                           |
| `MEMORY_PATH`                 | `domainchecker-memory.json` | `client/main/main.go` — where remembered decisions are saved to and loaded from                                          |
| `LOG_PATH`                    | `domainchecker-server.log` | `server/main/main.go` — where the server logs to (falls back to stderr if unwritable); never stdout, which is reserved for Squid's helper protocol |

`REQUEST_TIMEOUT_DURATION` should stay comfortably larger than
`USER_INPUT_TIMEOUT_DURATION` — it needs to leave room for the client's own
timeout-triggered denial to arrive before the server gives up and retries.

## Security considerations

- The WebSocket endpoint accepts connections from **any origin** and has
  **no authentication**. Anyone who can reach the server's port can connect
  as a client and start answering (or denying) requests. Only run this on a
  trusted network, or put it behind something that restricts access.
- A denied-by-default posture is used throughout: no connected client, a
  timed-out prompt, and a malformed Squid request all result in the
  request being blocked rather than allowed.
- The remembered-decisions file (`MEMORY_PATH`) is plain, unencrypted JSON
  listing every URL and domain a human has allowed or denied, written with
  `0600` permissions. Anyone who can read that file on disk can see (and,
  if they can write it, forge) a client's remembered browsing decisions.

## Known limitations

- If a request is retried while the original prompt is still showing on a
  client, that client can briefly show two overlapping prompts for the same
  URL; only the answer to the current attempt is honored server-side.
- If `MEMORY_PATH` exists but isn't valid JSON (e.g. it was hand-edited and
  broken, or got truncated by a crash mid-write), the client deletes it
  and logs a warning rather than refusing to start; all remembered
  decisions are lost in that case.
