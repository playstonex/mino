# OpenConnect/AnyConnect Integration Feasibility Analysis

> Date: 2026-05-15
> Target: mihomo (Meta Kernel) framework assessment

## Executive Summary

**The mihomo framework is architecturally ready** for an OpenConnect outbound adapter. It has a well-established L3 protocol integration pattern, proven by WireGuard and MASQUE (CONNECT-IP). The core work is in adapting `sslcon` from a "system VPN client" to a "mihomo internal L3 outbound", not in modifying mihomo itself.

---

## 1. Framework Capability Assessment

mihomo already has **everything needed** to host an OpenConnect outbound:

| Requirement | Status | Evidence |
|---|---|---|
| Pluggable outbound adapter interface | ✅ | `C.ProxyAdapter` in `constant/adapters.go` — clean interface with `DialContext`, `ListenPacketContext`, `IsL3Protocol`, `Close` |
| L3 protocol support (no DNS loopback) | ✅ | `IsL3Protocol() → true` triggers DNS pre-resolution in `tunnel/dns_dialer.go:87` |
| Internal gVisor network stack (no system TUN) | ✅ | `wireguard.NewStackDevice()` creates in-memory `stack.Stack` with TCP/UDP/ICMP — used by both WireGuard and MASQUE |
| Raw IP packet read/write bridge | ✅ | `StackDevice` bridges gVisor ↔ crypto layer via `outbound` channel + `dispatcher.DeliverNetworkPacket()` |
| Static type registration (no plugin system) | ✅ | Single `case "type":` in `adapter/parser.go` — 3 files to touch total |
| Dialer chain (proxydialer/slowdown/multiplex) | ✅ | `option.NewDialer(outbound.DialOptions())` provides the full dialer chain |

---

## 2. The Integration Pattern (Proven by 2 Protocols)

Both WireGuard and MASQUE follow the **exact same pattern**, which OpenConnect would replicate:

```
┌─────────────────────────────────────────────────────────┐
│ adapter/outbound/openconnect.go                         │
│                                                         │
│  OpenConnect struct {                                   │
│      *Base                                              │
│      tunDevice  wireguard.Device   ← reuse sing-wireguard │
│      ocSession *sslcon.VPNContext  ← the OpenConnect core │
│      option    OpenConnectOption                        │
│  }                                                      │
│                                                         │
│  DialContext()      → tunDevice.DialContext()            │
│  ListenPacket()     → tunDevice.ListenPacket()           │
│  IsL3Protocol()     → true                              │
│  Close()            → ocSession.Close() + tunDevice.Close()│
└─────────────────────────────────────────────────────────┘
         │                              │
         ▼                              ▼
┌──────────────────┐     ┌──────────────────────────────┐
│ StackDevice      │     │ sslcon core (adapted)        │
│ (sing-wireguard) │     │                              │
│                  │     │ Strip: setupTun, SetRoutes,  │
│ gVisor stack     │◄───►│ system TUN, netlink          │
│ wireEndpoint     │     │                              │
│ outbound channel │     │ Keep: CSTP/DTLS handshake,  │
│ DialContext()    │     │ TLS/DTLS transport,          │
│ ListenPacket()   │     │ PayloadIn/PayloadOut         │
└──────────────────┘     └──────────────────────────────┘
```

---

## 3. What Needs to Happen to `sslcon`

### sslcon Component Mapping

| sslcon Component | Action | mihomo Equivalent |
|---|---|---|
| `setupTun()` / system TUN creation | **Strip** | Replaced by `wireguard.NewStackDevice()` |
| `vpnc.SetRoutes()` / netlink | **Strip** | gVisor stack handles routing internally |
| `PayloadIn` / `PayloadOut` (raw IP packets) | **Bridge** | Connect to `StackDevice.Read()`/`Write()` via goroutines (like MASQUE does) |
| CSTP + DTLS handshake | **Keep as-is** | This IS the protocol — no equivalent in mihomo |
| TLS/DTLS transport layer | **Keep, adapt dialer** | Route through mihomo's `dialer` chain instead of direct `net.Dial` |
| Authentication (password/cert) | **Keep as-is** | Exposed via `OpenConnectOption` config fields |

---

## 4. Implementation Path (3 files + 1 fork)

### Files to touch in mihomo:

| File | Change |
|---|---|
| `constant/adapters.go` | Add `OpenConnect AdapterType = iota` (line ~50) + `String()` case |
| `adapter/outbound/openconnect.go` | **New file** (~400-500 lines). Struct, option, constructor, `DialContext`/`ListenPacketContext`/`Close`/`IsL3Protocol`, packet bridge goroutines |
| `adapter/parser.go` | Add `case "openconnect":` (~10 lines) |

### The Packet Bridge (Core Innovation)

Looking at how MASQUE does it (`masque.go:265-314`), the pattern is:

```go
// Outbound: TUN → sslcon → server
go func() {
    buf := pool.Get(pool.UDPBufferSize)
    defer pool.Put(buf)
    bufs := [][]byte{buf}
    sizes := []int{0}
    for ctx.Err() == nil {
        _, err := tunDevice.Read(bufs, sizes, 0)   // read from gVisor stack
        // ... handle err ...
        err = ocSession.SendPacket(buf[:sizes[0]])  // send via sslcon tunnel
    }
}()

// Inbound: server → sslcon → TUN
go func() {
    buf := pool.Get(pool.UDPBufferSize)
    defer pool.Put(buf)
    for ctx.Err() == nil {
        n, err := ocSession.RecvPacket(buf)          // recv from sslcon tunnel
        // ... handle err ...
        _, err = tunDevice.Write([][]byte{buf[:n]}, 0) // write to gVisor stack
    }
}()
```

### What to Fork/Vendor from sslcon

Only the **protocol layer**:
- CSTP (Cisco SSL Tunneling Protocol) handshake + framing
- DTLS negotiation (optional, via `pion/dtls`)
- Authentication flow (username/password/certificate)
- Session management (connect, reconnect, disconnect)

**NOT needed**:
- `setupTun` / `tun_dev` / system TUN management
- `vpnc.SetRoutes` / `netlink` / system route configuration
- Any platform-specific interface code

---

## 5. Outbound Adapter Architecture Reference

### ProxyAdapter Interface (`constant/adapters.go:118-144`)

```go
type ProxyAdapter interface {
    Name() string
    Type() AdapterType
    Addr() string
    SupportUDP() bool
    ProxyInfo() ProxyInfo
    MarshalJSON() ([]byte, error)
    DialContext(ctx context.Context, metadata *Metadata) (Conn, error)
    ListenPacketContext(ctx context.Context, metadata *Metadata) (PacketConn, error)
    SupportUOT() bool
    IsL3Protocol(metadata *Metadata) bool
    Unwrap(metadata *Metadata, touch bool) Proxy
    Close() error
}
```

### Adapter Type Constants (`constant/adapters.go:18-51`)

Current types: `Direct`, `Reject`, `Shadowsocks`, `VMess`, `VLESS`, `Trojan`, `Hysteria`, `Hysteria2`, `WireGuard`, `Tuic`, `SSH`, `Masque`, `P2P`, etc. OpenConnect would be added after the last value.

### Registration Pipeline

```
YAML Config                     config/config.go          adapter/parser.go
┌─────────────────┐             ┌──────────────────┐      ┌─────────────────┐
│ proxies:         │───parse──▶ │ parseProxies()    │─────▶│ ParseProxy()    │
│  - name: "myoc"  │            │ iterates          │      │ switch type     │
│    type: openconnect          │ cfg.Proxy []map   │      │   case "openconnect":│
│    server: ...   │            │ calls             │      │     Decode()     │
│    port: ...     │            │ adapter.ParseProxy│      │     NewOpenConnect()│
└─────────────────┘             └──────────────────┘      └────────┬────────┘
                                                                    │
                                                                    ▼
                                                        ┌──────────────────────┐
                                                        │ outbound.OpenConnect │
                                                        │  struct{ *Base }     │
                                                        │  + DialContext()     │
                                                        │  + ListenPacket()    │
                                                        │  + Close()           │
                                                        └──────────┬───────────┘
                                                                   │
                                          ┌────────────────────────┘
                                          ▼
                              ┌──────────────────────┐
                              │ adapter.NewProxy()    │── wraps with history, alive, JSON
                              │  → map[string]C.Proxy │── stored in Config.Proxies
                              └──────────────────────┘
```

### WireGuard Integration Architecture (Reference Implementation)

WireGuard in mihomo does **not** create a system TUN device. It creates a fully virtualized in-memory TCP/IP stack using gVisor's `stack.Stack`.

#### Packet Flow

```
TCP/UDP Dial (app → proxy)
  │
  ▼
StackDevice.DialContext() / ListenPacket()
  │  creates gonet.TCPConn/gonet.UDPConn on gVisor stack
  ▼
gVisor stack routes packets internally
  │
  ▼
wireEndpoint.WritePackets()  ← stack calls this to send
  │  pushes *stack.PacketBuffer onto outbound channel
  ▼
StackDevice.Read()  ← wireguard-go calls this (as tun.Device)
  │  reads from outbound channel
  ▼
wireguard-go encrypts the IP packet
  │
  ▼
ClientBind.Send()  ← sends encrypted UDP datagram to server
  │  via mihomo's dialer chain (proxydialer/slowdown)
  ▼
[Internet] ── WireGuard server

  ▲
[Reverse path]
  ▲
ClientBind.receive()  ← reads encrypted UDP from server
  ▲
wireguard-go decrypts → calls Device.Write()
  ▲
StackDevice.Write()  ← creates stack.PacketBuffer, calls
  ▲  dispatcher.DeliverNetworkPacket()
  ▲
gVisor stack delivers to original gonet.TCPConn/UDPConn
```

#### Key Files

| File | Role |
|---|---|
| `adapter/outbound/wireguard.go` | Adapter: constructor, `DialContext`, `ListenPacketContext`, `IsL3Protocol`, lazy init |
| `adapter/outbound/masque.go` | Second L3 adapter using same `wireguard.Device` pattern (CONNECT-IP) |
| `sing-wireguard/device_stack.go` | `StackDevice`: gVisor `stack.Stack` + `wireEndpoint` bridge |
| `sing-wireguard/gonet.go` | `DialTCPWithBind()`, `gonet.TCPConn`/`UDPConn` on gVisor stack |
| `tunnel/dns_dialer.go:87` | L3 DNS pre-resolution to prevent loopback |

---

## 6. Scope Assessment

| Scope | Difficulty | Notes |
|---|---|---|
| ocserv/OpenConnect server + password/cert + CSTP + optional DTLS | **Medium** | Well-defined scope, `sslcon` provides 80% of the code |
| Cisco AnyConnect SAML/MFA | **High** | Browser-based auth flows, needs deep reverse-engineering per gateway |
| Full enterprise AnyConnect gateway compatibility | **Very High** | Diverse implementations, proprietary extensions |

---

## 7. Dependencies & Compatibility

| Item | Status |
|---|---|
| Go version | `sslcon` requires Go 1.24.2, mihomo is Go 1.24.0 — minor alignment needed |
| `pion/dtls` | Already in mihomo's dependency tree (via `tuic`) — ✅ no new dep |
| `wireguard.NewStackDevice` | Reusable as-is from sing-wireguard — ✅ |
| gVisor | Already included via `with_gvisor` build tag — ✅ |
| `netlink` / `wintun` from sslcon | **Not needed** — we strip system TUN code |

---

## 8. Conclusion

**The mihomo framework requires zero modifications** to support OpenConnect. The outbound adapter architecture, L3 protocol handling, gVisor internal stack, and dialer chain are all battle-tested by WireGuard and MASQUE. The effort is 100% in adapting `sslcon`:

1. Fork `sslcon` → strip system TUN/routing code → expose raw IP packet I/O
2. Create `adapter/outbound/openconnect.go` following the MASQUE pattern exactly
3. Bridge sslcon's packet I/O to `wireguard.NewStackDevice()` via two goroutines
4. Register in `adapter/parser.go`

**Estimated effort**: Medium (2-3 weeks for a working OpenConnect/ocserv implementation; enterprise AnyConnect is a separate, much larger project).

---

## Appendix: Candidate Libraries

| Library | URL | License | Notes |
|---|---|---|---|
| `tlslink/sslcon` | https://github.com/tlslink/sslcon | MIT | Go OpenConnect VPN client, core of AnyLink Secure Client |
| `WarrDoge/sslcon` | https://github.com/WarrDoge/sslcon | MIT | More library-friendly (`lib.VPNContext`), Go 1.24.2, pkg.go.dev listed |
