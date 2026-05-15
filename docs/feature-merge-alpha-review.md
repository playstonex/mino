# feature/merge-alpha Review

Base branch: `develop`

Merge base: `b62de1b4f5ffcb50cc633958da20b05b12c46549`

Review command:

```bash
git diff b62de1b4f5ffcb50cc633958da20b05b12c46549
```

## Merge Recommendation

Do not merge to `develop` yet.

The branch compiles and passes tests when vet is disabled, but it still has one merge-blocking correctness issue and several smaller actionable fixes. The P1 issue can cause process-based routing rules to evaluate against the wrong process on Linux.

## Findings

### [P1] Non-matching netlink socket can be returned as success

File: `component/process/process_linux.go`

Line: `144`

`resolveSocketByNetlink` clears `err` before the response is proven to match the requested source IP and port. If the netlink dump contains entries but none match, the function returns the last non-matching UID/inode with `nil` error. That can make process-based rules match the wrong process instead of falling back or returning `ErrNotFound`.

Recommendation: only assign the success UID/inode and clear `err` after both source port and source IP match.

### [P2] Hysteria2 outbound realm resolves IPv6-only requests as IPv4

File: `adapter/outbound/hysteria2.go`

Line: `289`

The IPv6-only branch calls `resolver.LookupIPv4WithResolver`, so realm/STUN resolution cannot return AAAA records when the caller requested IPv6 only.

Recommendation: call `resolver.LookupIPv6WithResolver` in the `ipv6 && !ipv4` branch.

### [P2] Hysteria2 inbound realm resolves IPv6-only requests as IPv4

File: `listener/sing_hysteria2/server.go`

Line: `188`

The inbound realm resolver has the same IPv6-only branch bug as the outbound path.

Recommendation: call `resolver.LookupIPv6WithResolver` in the `ipv6 && !ipv4` branch.

### [P2] WebSocket early-data write deadline updates read deadline

File: `transport/vmess/websocket.go`

Line: `288`

`SetWriteDeadline` forwards to `SetReadDeadline` after the websocket is dialed. Callers that set write deadlines only affect reads, so writes may block indefinitely under a stalled peer.

Recommendation: call `wsedc.conn.SetWriteDeadline(t)`.

### [P2] post-up failure can skip executor shutdown

File: `main.go`

Line: `199`

`post-up` runs after `hub.Parse` has started listeners and TUN state, but `defer executor.Shutdown()` is registered only after `post-up` succeeds. If the script fails, `log.Fatalln` exits immediately and cleanup is skipped.

Recommendation: register `defer executor.Shutdown()` before running `post-up`, or avoid fatal exit after runtime resources are started.

## Verification

```bash
go test ./...
```

Result: failed because vet reports non-constant format string diagnostics in unchanged packages:

- `log/sing.go`
- `common/structure/structure.go`

```bash
go test -vet=off ./...
```

Result: passed.

Additional sanity checks:

```bash
go test -vet=off -tags no_tailscale ./adapter ./adapter/outbound ./dns
GOOS=android GOARCH=arm64 go test -vet=off -run '^$' ./component/iface/anet ./component/iface
GOOS=linux GOARCH=amd64 go test -vet=off -run '^$' ./component/process
```

Result: passed.
