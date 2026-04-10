# iOS/macOS 平台优化记录

本文档记录了 Mate xcframework 在 iOS/macOS Network Extension 环境下的性能与内存优化，以及相关 Bug 修复。

---

## 一、内存管理

### 问题

iOS Network Extension 进程有严格的内存上限（iOS 15+ 约 50 MB）。Go runtime 默认不感知这个限制，GC 可能在内存接近上限时才触发，导致进程被系统 OOM kill。

### 修复

在 `mate/service.go` 的 `init()` 中按平台动态配置 Go runtime：

```go
switch runtime.GOOS {
case "ios":
    runtimeDebug.SetMemoryLimit(40 * 1024 * 1024) // 40 MB heap 上限
    runtime.GOMAXPROCS(3)                          // 限制 OS 线程数
    runtimeDebug.SetGCPercent(50)                  // 更积极的 GC

case "darwin":
    // macOS 无内存约束，使用 Go 默认值，不做限制

default:
    runtimeDebug.SetMemoryLimit(40 * 1024 * 1024)
    runtime.GOMAXPROCS(3)
    runtimeDebug.SetGCPercent(50)
}
```

- `runtime.GOOS` 在 gomobile 编译 iOS target 时值为 `"ios"`，编译 macOS target 时值为 `"darwin"`，可以精确区分。
- 40 MB 为 Go heap 上限，为 C/ObjC 运行时、goroutine 栈、系统调用缓冲留出约 10 MB 余量。
- macOS 上不设任何限制，充分利用系统资源。

---

## 二、加密性能优化

### 问题

`overlay_manager.go` 的 `encryptPacket` / `decryptPacket` 每次调用都执行：

```go
block, _ := aes.NewCipher(sharedKey)  // 每包重建 AES key schedule
gcm, _   := cipher.NewGCM(block)      // 每包重建 GCM 实例
```

在高流量场景（SSH 文件传输、视频流）下，每秒数千个包，每包 2 次 crypto 初始化，CPU 和 GC 压力极大。

### 修复

在 `OverlayManager` 中增加 `peerCiphers map[string]cipher.AEAD`，在 `deriveAllPeerKeys` 时一次性创建并缓存每个 peer 的 `cipher.AEAD` 实例：

```go
func newAEAD(key []byte) (cipher.AEAD, error) {
    block, err := aes.NewCipher(key)
    if err != nil { return nil, err }
    return cipher.NewGCM(block)
}
```

加密/解密时直接使用缓存的 `cipher.AEAD`，不再重建。

同时优化 `encryptPacket` 的内存分配，利用 `gcm.Seal` 的 append 语义，从 5 次分配降到 2 次：

```go
// 旧：nonce(1) + Seal(1) + result(1) = 3 次分配
// 新：nonce(1) + Seal 追加到 nonce slice(0 额外分配) = 1 次分配
return gcm.Seal(nonce, nonce, plaintext, nil), nil
```

---

## 三、Buffer Pool（TUN 读写复用）

### 问题

TUN read loop 每个数据包都分配新 buffer：

```go
buf := make([]byte, 65535)          // 每次循环重新分配（实际是循环外，但 packet 是每包分配）
packet := make([]byte, n-utunHeaderLen) // 每包分配
```

`writeToTUN` 也是每包 `make([]byte, 4+len(packet))`。

### 修复

使用 `sync.Pool` 复用 TUN 读写 buffer：

```go
var tunReadPool  = sync.Pool{New: func() any { b := make([]byte, 65535); return &b }}
var tunWritePool = sync.Pool{New: func() any { b := make([]byte, 4+65535); return &b }}
```

读循环从 pool 取 buffer，处理完后归还；`writeToTUN` 同样从 pool 取写 buffer，写完后归还。减少 GC 压力，降低延迟抖动。

---

## 四、日志优化

### 问题

`overlay_manager.go` 有 60+ 处 `m.logf` 调用，其中大量在热路径上（每个数据包都 log）：

```
[Overlay-Go] routing 1400-byte TCP 100.96.0.10 → to 100.96.0.11 via 11a7562a
[Overlay-Go] Decrypted 1400-byte packet from 11a7562a
```

这些日志通过 `platform.WriteLog` → Swift `FileHandle.write` 写入磁盘，是同步 I/O，会阻塞 Go goroutine，在高流量下严重影响吞吐量。

### 修复

- 移除所有 per-packet 日志（routing、decrypt、send 路径）。
- 启动后 30 秒自动关闭 debug 级别日志（通过 `debugPacketLog atomic.Bool` 控制）：

```go
m.debugPacketLog.Store(true)
go func() {
    time.Sleep(30 * time.Second)
    m.debugPacketLog.Store(false)
}()
```

- `overlay_transport.go` 的 `Send` / `dispatchPacket` 移除 per-packet 日志。
- TUN read loop 心跳日志间隔从 5 秒延长到 10 秒。

---

## 五、`seenCandidates` 无限增长

### 问题

每个 peer 的每个 signaling candidate 都记录在 `seenCandidates[peerID][sigKey]` 中，只在 peer 被移除时清理。长时间运行、P2P 反复重试时，这个 map 持续增长，占用内存。

### 修复

在 `markSeen` 中加入 `maxSeenPerPeer = 200` 上限，超出时清除最旧的一半：

```go
const maxSeenPerPeer = 200

func (m *OverlayManager) markSeen(peerID, sigKey string) {
    seen := m.seenCandidates[peerID]
    if len(seen) >= maxSeenPerPeer {
        count := 0
        for k := range seen {
            delete(seen, k)
            if count++; count >= maxSeenPerPeer/2 { break }
        }
    }
    seen[sigKey] = true
}
```

---

## 六、`deriveAllPeerKeys` 冗余计算

### 问题

`refreshOverlayRuntime` 每 20 秒调用一次 `deriveAllPeerKeys`，对所有 peer 执行 X25519 + HKDF-SHA256，即使 peer 列表没有变化。

### 修复

只在 peer 列表发生变化时重新派生密钥：

```go
peersChanged := !mapsEqual(m.knownPeerIDs, newPeerIDs)
m.buildRouteTable()
if peersChanged {
    m.deriveAllPeerKeys()
}
```

同时 `deriveAllPeerKeys` 内部也做了增量检查：如果 peer 的公钥未变，直接复用已有的 `cipher.AEAD`，不重新计算。

---

## 七、Goroutine WaitGroup 竞态修复

### 问题

`signalingLoop` 和 `tunReadLoop` 在 goroutine 内部调用 `m.wg.Add(1)`，如果 `cancelCh` 在 goroutine 启动前就被 close（极端竞态），`wg.Wait()` 可能在 goroutine 实际退出前就返回，导致资源泄漏。

### 修复

将 `wg.Add(1)` 移到 `go` 语句之前：

```go
// 修复前
go m.signalingLoop()  // signalingLoop 内部 wg.Add(1)

// 修复后
m.wg.Add(1)
go m.signalingLoop()  // signalingLoop 内部只有 defer wg.Done()
```

---

## 八、`relayPacketConn.ReadFrom` 永远阻塞

### 问题

`adapter/outbound/p2p.go` 的 `relayPacketConn.ReadFrom` 实现为：

```go
func (r *relayPacketConn) ReadFrom(b []byte) (n int, addr net.Addr, err error) {
    select {} // 永远阻塞！
}
```

这导致通过 relay fallback 的 P2P outbound adapter 无法读取响应数据，所有 TCP 连接超时。

### 修复

给 `relayPacketConn` 增加 `recvCh chan []byte`，relay client 的 receive handler 通过全局 `relayReceivers map` 将数据分发到对应的 conn：

```go
type relayPacketConn struct {
    peerID    string
    recvCh    chan []byte   // 接收通道
    closed    chan struct{}
    closeOnce sync.Once
}

func (r *relayPacketConn) ReadFrom(b []byte) (n int, addr net.Addr, err error) {
    select {
    case <-r.closed:
        return 0, nil, io.EOF
    case data := <-r.recvCh:
        return copy(b, data), &net.UDPAddr{}, nil
    }
}
```

relay client 创建时注册 receive handler：

```go
rc.SetReceiveHandler(func(peerID string, data []byte) {
    relayReceiversMu.Lock()
    receivers := relayReceivers[peerID]
    relayReceiversMu.Unlock()
    for _, r := range receivers {
        select {
        case r.recvCh <- dataCopy:
        default: // 满了则丢弃
        }
    }
})
```

---

## 九、`NewP2P` relay 创建竞争

### 问题

每个 P2P outbound 实例（每个 peer 一个）在构造时都启动一个 goroutine 尝试连接 relay，N 个 peer 就有 N 个 goroutine 竞争创建同一个全局 `relayClient`，浪费 N-1 个连接。

### 修复

使用 `sync.Once` 保证只有一个 goroutine 创建 relay：

```go
var relayOnce sync.Once

go func() {
    relayOnce.Do(func() {
        ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
        defer cancel()
        _, _ = p.getRelayPacketConn(ctx)
    })
    // 注册本 peer（relay 已存在时直接 AddPeer）
    relayMu.Lock()
    if relayClient != nil { relayClient.AddPeer(p.peerID) }
    relayMu.Unlock()
}()
```

`P2P.Close` 时重置 `relayOnce`，允许下次 Start 重新创建：

```go
func (p *P2P) Close() error {
    relayMu.Lock()
    defer relayMu.Unlock()
    if relayClient != nil {
        relayClient.Close()
        relayClient = nil
        relayOnce = sync.Once{} // 允许重启后重新创建
    }
    return nil
}
```

---

## 十、DataChannel 丢包静默

### 问题

`DataChannelConn` 和 `PacketDataChannelConn` 的 channel 满时静默丢弃数据，没有任何日志，难以排查问题。`PacketDataChannelConn` 的 channel 容量只有 256，在 UDP 突发场景下容易满。

### 修复

- `PacketDataChannelConn` channel 容量从 256 提升到 1024。
- 两个 conn 丢包时记录计数，每 100 次打印一次日志：

```go
dropped := &atomic.Int64{}
// ...
default:
    if dropped.Add(1)%100 == 1 {
        fmt.Fprintf(os.Stderr, "[DataChannelConn] readCh full, dropped %d packets\n", dropped.Load())
    }
```

---

## 十一、`relayToLocalTCP` 日志清理

### 问题

`mate/p2p.go` 的 `relayToLocalTCP` 使用 `fmt.Fprintf(os.Stderr, ...)` 打印日志，这些日志不经过 `platform.WriteLog` 路径，在 iOS Network Extension 中不可见，且包含冗余的 relay/connect 成功信息。

### 修复

移除所有 `fmt.Fprintf(os.Stderr, ...)` 调用，保留错误处理逻辑。

---

## 十二、`configSample` YAML 模板精简（Swift 侧）

### 问题

`Violet/managers/ConnectManager.swift` 中的 `configSample` 包含约 80 行中文注释，每次生成配置时 Yams 都要解析这些注释，增加不必要的解析开销和内存占用。

### 修复

移除所有注释，只保留必要的配置字段，模板从 ~130 行缩减到 ~50 行。

---

## 变更文件汇总

| 文件 | 类型 | 说明 |
|------|------|------|
| `mate/service.go` | 优化 | 按平台动态配置内存限制、GOMAXPROCS、GCPercent |
| `mate/overlay_manager.go` | 优化+修复 | cipher 缓存、buffer pool、日志精简、seenCandidates 上限、wg 竞态、deriveAllPeerKeys 增量 |
| `mate/overlay_transport.go` | 优化 | 移除 per-packet 日志 |
| `mate/p2p.go` | 修复 | 移除 relayToLocalTCP stderr 日志 |
| `transport/p2p/manager.go` | 修复+优化 | DataChannelConn 丢包日志、PacketDataChannelConn 容量提升 |
| `adapter/outbound/p2p.go` | 修复 | relayPacketConn.ReadFrom 阻塞、relay 创建竞争、relayOnce 重置 |
| `Violet/managers/ConnectManager.swift` | 优化 | configSample 精简 |
