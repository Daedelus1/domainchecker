# Deployment plan: domainchecker as the egress-proxy's `external_acl_type` helper

Target: `~/Documents/Configuration/nixos/modules/ai-stack/egress-proxy/`

This plan only describes changes to make there. **Nothing in that directory, or
anywhere outside this repo, has been edited.** All diffs below are
illustrative/proposed, for review before anyone touches the real files.

## 1. What's there now

The egress-proxy module runs Squid in a Docker container (managed by a NixOS
`composeProjects` module), sitting between an AI agent container (on the
internal `egressnet` network) and the real internet (on `outbound`):

- **`squid.conf`**: one `http_port 3128`, one `external_acl_type` calling
  `/usr/local/bin/mvp-checker` with only `%SRC` (no `%URI`), `ttl=0
  negative_ttl=0` (never cache a verdict — always re-ask).
- **`mvp-checker.go`**: the actual current helper. Ignores its stdin entirely
  and flips a coin per line. A placeholder, not a real control.
- **`egress-proxy.Dockerfile`**: multi-stage — builds `mvp-checker.go` with
  `golang:1.23`, copies the single resulting binary into an
  `ubuntu/squid:latest` final image. Also installs `squid-openssl` and
  pre-creates `ssl_ca`/`ssl_db` directories, and its comments mention
  "harness's trust of it" regarding a CA cert — i.e. SSL-bump (MITM'ing HTTPS
  to see full decrypted URLs) was anticipated but is **not actually configured**
  in the `squid.conf` shown; CONNECT traffic today is a plain opaque tunnel.
- **`entrypoint.sh`**: fixes ownership, initializes Squid's cache dir, execs
  `squid -N -f`. Not currently wired up — the Dockerfile's `ENTRYPOINT` lines
  are both commented out, so the base image's own default entrypoint runs
  instead.
- **`domain-admin.go`** + the `8089` port + `persist/{allowlist,denylist,decisions}.txt`:
  a sketch of a *different* design — an HTTP control API (`/pending`,
  `/decide`) that a human CLI polls. `mvp-checker.go` never implements that
  API, and nothing reads the `persist/` files. This is dead scaffolding for
  an approach domainchecker supersedes entirely (domainchecker is push-based
  over a WebSocket + its own client-side JSON memory, not a polled REST
  queue). Recommend retiring `domain-admin.go`, the `8089` mapping, and
  `persist/` once this swap lands — not done as part of this plan, called out
  so it isn't mistaken for still-relevant infrastructure.
- **`docker-compose.yaml`**: builds the above Dockerfile, mounts
  `squid.conf` read-only and two writable volumes, joins `egressnet`
  (reachable by the AI agent) and `outbound` (the only real route out), and
  publishes `127.0.0.1:8089:8089` — **host-loopback only**, deliberately never
  on `egressnet`, so the admin surface can't be reached by the thing being
  controlled. This is exactly the pattern domainchecker's own WebSocket port
  needs too (see §4).
- **`flake.nix`**: wires the above into a NixOS `services.composeProjects`
  module via `etcCopies` (individual files copied into `/etc/compose/...`)
  and one `etcSymlinks` entry (`persist`, a whole directory symlinked in).

## 2. Critical finding: Squid's CONNECT `%URI` breaks domain-scoped memory

This is the one piece of real engineering risk in this plan, verified
directly rather than assumed, so it's called out before anything else.

For a plain (non-TLS) proxied HTTP request, Squid's `%URI` is a full URI with
scheme (`http://example.com/path`) — domainchecker already handles this
correctly and is tested against it.

For an HTTPS request, the browser/agent issues `CONNECT host:port` and Squid
(without SSL-bump) can't see anything past that — `%URI` is just `host:port`,
with no scheme and no `//`. That's virtually all of an AI agent's real
egress: API calls, package registries, git over HTTPS, etc.

Go's `net/url.Parse` does not error on `"api.anthropic.com:443"` — it
succeeds, but mis-parses it:

```
raw="api.anthropic.com:443" -> err=<nil> scheme="api.anthropic.com" opaque="443" host="" path="" hostname=""
raw="pypi.org:443"           -> err=<nil> scheme="pypi.org"          opaque="443" host="" path="" hostname=""
raw="github.com:443"         -> err=<nil> scheme="github.com"        opaque="443" host="" path="" hostname=""
```
(confirmed by actually running this against the Go stdlib, not from memory)

Consequences, traced through `server/domainchecker/domainchecker.go`'s
`handleStdio` and `client/memory.go`'s `decisionMemory`:

- It does **not** hit the `BH message=URL_Decoding_Failed` path — `url.Parse`
  didn't return an error, so this fails silently rather than loudly.
- `url.URL.String()` happens to round-trip back to the original text
  (`"api.anthropic.com:443"`), so the human-facing prompt
  (`Allow "api.anthropic.com:443"?`) still reads correctly, and **URL-scoped**
  remembering (`yu`/`nu`, keyed on `u.String()`) still works correctly and
  without collisions, since the full `host:port` string is unique per target.
- `url.URL.Hostname()` returns `""` for **every** CONNECT-style input,
  regardless of the actual host. **Domain-scoped remembering (`yd`/`nd`) is
  broken for all HTTPS traffic**: it's keyed on `Hostname()`, so a domain
  decision recorded for one HTTPS host is silently recorded under the same
  empty-string key as every other HTTPS host, and will incorrectly apply to
  all of them.

### Fix options (pick one before relying on `yd`/`nd` in this deployment)

1. **Code fix in domainchecker — done.** `handleStdio`
   (`server/domainchecker/domainchecker.go`) now detects a bare `host:port`
   value (no `"://"` present) and prefixes it with `https://` before handing
   it to `url.Parse`, so `Hostname()` returns the real host instead of `""`.
   Covered by `TestHandleStdioConnectStyleURIGetsHostname`. `yd`/`nd` is now
   safe to use for HTTPS targets.
2. **Infra fix: enable SSL-bump in Squid.** The Dockerfile's half-finished
   `ssl_ca`/`ssl_db` setup and its CA-cert-trust comment suggest this was
   already anticipated for this exact proxy. If Squid decrypts and re-signs
   HTTPS, `%URI` becomes a real `https://host/path` URL and domainchecker
   needs no change at all. This is strictly more work (generating/rotating a
   CA cert, and getting whatever's inside `egressnet` — the AI agent's HTTP
   client — to trust it) and is security-sensitive in its own right (it's a
   deliberate MITM). Treat as a separate, parallel initiative, not a
   prerequisite for this swap.
3. **Interim/no-code-change mitigation** (no longer needed, now that (1) is
   done): until (1) shipped, the human reviewer would have needed to use
   `yu`/`nu` (remember this exact `host:port`) instead of `yd`/`nd` for
   HTTPS traffic.

## 3. Packaging: getting the binary built and into the image

domainchecker is a multi-package Go module (`go.mod` at the repo root,
`./server/main`, `./client/main`), unlike `mvp-checker.go`'s single file. The
Dockerfile's build context today is the egress-proxy directory itself
(`context: .` in `docker-compose.yaml`), which has no visibility into this
repo at all — these are unrelated directory trees
(`~/Documents/Configuration/...` vs `~/Documents/Programming/Go/...`). This
is the main open decision in this plan.

**Option A — vendor/symlink the source tree in (fast path).**
Add a flake input (or a plain `etcSymlinks` entry, following the exact
precedent already set by `persist`) pointing at this repo, so the whole
source tree lands at a path the Dockerfile's build context can see, e.g.
`/etc/compose/egress-proxy/domainchecker-src`. Extend the Dockerfile's
builder stage:

```dockerfile
FROM golang:1.23 AS build
WORKDIR /src
COPY domainchecker-src/go.mod domainchecker-src/go.sum ./
RUN go mod download
COPY domainchecker-src/ .
RUN CGO_ENABLED=0 go build -o /domainchecker-server ./server/main
```

Fast to stand up, matches the existing file-copying convention in
`flake.nix`, but couples the NixOS module to a mutable absolute host path (or
a git-based flake input you'd still need to add) rather than a pinned,
reproducible build — a bit of an odd fit for an otherwise Nix-first stack.

**Option B — package domainchecker as its own flake output (recommended,
more work).**
Add a `flake.nix` to *this* repo exposing the server binary via
`buildGoModule` (not done in this plan — a separate, follow-up task in this
repo). `egress-proxy/flake.nix` then takes that as a flake input and copies
the resulting Nix store path's binary straight into the final image,
skipping the Go build stage in the Dockerfile entirely:

```dockerfile
FROM ubuntu/squid:latest
COPY domainchecker-server /usr/local/bin/domainchecker-server
```

Reproducible, pinned, consistent with how the rest of `ai-stack` is built —
but is strictly more work and isn't needed to validate the integration.

**Recommendation:** prototype with Option A to validate the whole chain
end-to-end against the real stack, then migrate to Option B before
considering this "done" rather than "a working hack."

Either way, the client binary (`domainchecker-client`) is **not** built into
this image at all — see §5.

## 4. `squid.conf` changes

```diff
 http_port 3128

-external_acl_type request_check children-max=1 children-startup=1 ttl=0 negative_ttl=0 %SRC /usr/local/bin/mvp-checker
-acl request_allowed external request_check
+external_acl_type domain_checker children-max=1 children-startup=1 ttl=0 negative_ttl=0 %SRC %URI /usr/local/bin/domainchecker-server
+acl request_allowed external domain_checker

 http_access allow request_allowed
 http_access deny all
```

Notes:

- **`%URI` must be added.** domainchecker's `handleStdio` requires at least
  two whitespace-separated fields per line (`<client-ip> <encoded-url>`); the
  current config only sends `%SRC`, so every single line would hit the
  `ERR message=Invalid_Input_Format` path and `MakeRequest` would never be
  called. `mvp-checker.go` never noticed because it ignores its input
  entirely.
- **`children-max=1 children-startup=1` can stay exactly as-is** — this
  already matches domainchecker's own requirement (it owns a fixed TCP
  listener for the WebSocket and a single serial stdin stream, so more than
  one running instance would collide on port `:8080`).
- **Keep `ttl=0 negative_ttl=0`, don't copy the README's `ttl=3600
  negative_ttl=10` recommendation.** That recommendation is right for casual
  interactive use (don't re-prompt a human for an hour), but in a sandboxed
  AI-agent context you likely want every request to actually reach the
  helper, not have Squid silently reuse an hour-old verdict without
  consulting it again. domainchecker's own client-side memory (`yu/nu/yd/nd`,
  now persisted to disk) already gives the "don't re-prompt for things
  already decided" behavior, just one layer further in — the combination of
  Squid-level `ttl=0` (always ask) + domainchecker-level memory (instant
  answer if already decided) gets freshness and snappiness at the same time.
- Optionally add `deny_info` + a custom error template to surface
  denomainchecker's `DenyMessage` to the agent's HTTP response body (see this
  repo's README → "Surfacing denial reasons", and
  `squid/ERR_CUSTOM_ACCESS_DENIED.html` / `squid/squid.conf` in this repo for
  a ready-made example). Lower priority here than for a human-browser use
  case — the "end user" being blocked is an AI agent process, not a person
  reading an error page — but still useful for anyone later grepping Squid's
  access/cache log for *why* something was denied.

## 5. `docker-compose.yaml` changes

```diff
     ports:
       # admin control API -- host-only, never exposed to claude-code's network
-      - "127.0.0.1:8089:8089"
+      - "127.0.0.1:8089:8089" # dead: domain-admin.go/mvp-checker's old control API, retire separately
+      # domainchecker's WebSocket control channel -- host-loopback only,
+      # mirroring the 8089 pattern above. Must NEVER be reachable from
+      # egressnet: the thing on the other side of this port decides whether
+      # the agent gets network access at all, and the server accepts any
+      # Origin (see this repo's README's Security considerations), so the
+      # loopback-only bind is the actual security boundary, not any check
+      # the server does itself.
+      - "127.0.0.1:8080:8080"
```

- Don't use `expose:` for the new port — that publishes to other containers
  on the same compose network (`egressnet`), which is exactly what must be
  avoided here. `ports: - "127.0.0.1:8080:8080"` is the right form, matching
  what's already done for `8089`.
- The `squid_config`/`persist` volumes and the `outbound`/`egressnet`
  networks don't need to change.
- Once domainchecker is confirmed working, consider dropping the `8089`
  mapping (and `domain-admin.go` from `etcCopies`, and the `persist`
  `etcSymlinks` entry) — see §1.

## 6. `entrypoint.sh` / container startup

No change needed to how Squid itself starts. Squid spawns the
`external_acl_type` helper as its own managed child process — exactly the
same mechanism already in use for `mvp-checker` — so `domainchecker-server`
doesn't need its own supervisor, init system, or second container. The one
thing to decide is whether to finally uncomment `entrypoint.sh`'s use (it's
currently dead — the Dockerfile's `ENTRYPOINT` lines are both commented out)
or continue relying on the base image's default; unrelated to this swap,
flagging only because it was found while reading these files.

## 7. The human's client — runs outside this container entirely

`domainchecker-client` is a terminal program for a human, not a container
service. It should run wherever the human reviewing requests actually is —
most likely the NixOS host itself, in a normal terminal, left running:

```sh
go build -o domainchecker-client ./client/main   # built from this repo
./domainchecker-client
```

- It dials `ws://localhost:8080/ws` by default (`client/main/main.go`'s
  `SERVER_URI`/`ORIGIN_URI` constants) — which, given the `127.0.0.1:8080:8080`
  mapping in §5, Just Works from the host with zero code changes.
- For remote review (phone, laptop elsewhere), tunnel instead of exposing the
  port further: `ssh -L 8080:127.0.0.1:8080 <nixos-host>`, then run the
  client locally against `localhost:8080` as normal. Never widen the
  `docker-compose.yaml` port binding beyond `127.0.0.1` to achieve this.
- `SERVER_URI`/`ORIGIN_URI`/`MEMORY_PATH` are compile-time Go constants, not
  flags or env vars. Fine for a single fixed deployment; worth turning into
  CLI flags later if this ever needs to point at more than one stack or a
  non-default port (not in scope for this plan).
- Point `MEMORY_PATH` (the new remembered-decisions JSON file, see this
  repo's `client/memory.go`) at an ordinary path on the host, e.g.
  `~/.local/state/domainchecker/memory.json` — this is host-side state, not
  container state, and needs no volume mount or NixOS module change at all.
- **Operational requirement**: as long as domainchecker-client isn't running
  and connected, every single outbound request from the AI agent is denied
  immediately (fail-closed by design — see this repo's README). The human's
  client needs to be up whenever the agent stack is expected to have any
  egress at all.

## 8. Known UX note (not a blocker)

If the agent fires off several requests concurrently (e.g. installing
multiple packages, several parallel API calls), each gets its own prompt on
the client, each handled by its own goroutine already (so none of them block
each other or heartbeats) — but their `Allow "..."? ` prompts can interleave
on the same terminal. Functionally correct (each is tracked by its own
`RequestID`/`Attempt`), just a terminal-UX wrinkle the reviewer should expect
going in.

## 9. Suggested rollout order

1. ~~Fix the CONNECT/`Hostname()` bug in domainchecker (§2, option 1) and add
   a regression test asserting `Hostname()` is non-empty for a
   `host:port`-only input.~~ **Done** — see §2.
2. Pick a packaging option (§3) and get `domainchecker-server` building
   inside `egress-proxy.Dockerfile`, alongside (not yet replacing)
   `mvp-checker`, so both binaries exist in the image during validation.
3. Update `squid.conf` (§4) and `docker-compose.yaml` (§5) in a scratch/test
   checkout of the nixos config — not the live one — and rebuild the compose
   project there.
4. Start `domainchecker-client` on the host, confirm it connects, and drive
   both an HTTP and an HTTPS target through the proxy by hand, confirming:
   - the prompt text is sensible for both,
   - `y`/`n` (one-off) works for both,
   - `yu`/`nu` works for both,
   - `yd`/`nd` works for both (safe now that step 1's fix has landed).
5. Confirm fail-closed behavior: stop the client, confirm new requests are
   denied immediately rather than hanging.
6. Only then, swap `squid.conf`'s helper path to `domainchecker-server` for
   real and drop `mvp-checker` from the Dockerfile.
7. Retire `domain-admin.go`, the `8089` mapping, and `persist/` (§1), in a
   follow-up change.

## 10. Rollback

Keep `mvp-checker.go` and the original `squid.conf` line in git history (no
need to delete them as part of the swap commit). Rolling back is reverting
`squid.conf`'s `external_acl_type` line and rebuilding — Squid picks up a
new helper on `squid -k reconfigure` without needing a full container
restart.
