# Violet iOS Network Extension 内存与吞吐调优技术手册

> 一次跨越多轮、由 profile 驱动的 iOS Network Extension（NE）内存杀进程排查的完整记录。
> 目标读者：接手 Violet / mihomo 移动端内存问题的工程师。
> 结论：iPhone 12 / 日本 hysteria2 节点，下载 **160–200 Mbps**，直接运行不崩；
> Xcode debug 附着时仍会因调试器开销触及 50 MB 上限。

相关提交（mihomo `develop`）：`509c613a` · `04d2ddb4` · `6f9c3a09` · `f7eb2256` · `de25e2ea`
及后续窗口调参；sing-tun `meta`：合并上游 `15dd90c`。

---

## 第一部分：问题域

### 1.1 iOS Network Extension 的内存约束

iOS 给 `NEProvider` 子类扩展（我们的 `ProxyTunnel.appex`）分配约 **50 MB** 的
`phys_footprint` 硬上限。超过即被内核 Jetsam 以
`EXC_RESOURCE (RESOURCE_TYPE_MEMORY, limit=50 MB)` / SIGKILL 9 杀死，错误串为
`Terminated due to memory issue`。

这 50 MB 是**整个进程**的：Go runtime（堆 + 栈 + runtime 常驻）、用户态 gVisor TCP 栈、
Swift/ObjC runtime、所有线程栈，全部算在内。其中 Go runtime 的常驻开销通常比 live heap
高出 **19–24 MB**，这个差值是后面所有分析的关键。

### 1.2 症状

真机高速下载（speedtest）时 NE 被杀，表现为双峰：
- 下载**要么冲到 100+ Mbps 然后崩溃**，要么**塌到 <1 Mbps**，中间没有稳定档；
- 上传始终正常（~30 Mbps）。

---

## 第二部分：方法论——为什么前几轮都错了

### 2.1 "压一项、无界项搬家" 陷阱

前四轮每一轮都在**读代码猜"是哪个缓冲/池太大"**，然后压它，结果：

| 轮次 | 压的项 | 结果 |
|---|---|---|
| 1 | gVisor TCP 窗口 / MTU | MTU 2048 把 gVisor chunk 池 12→2.85 MB，榜首换成 quic-go StreamFrame 池 |
| 2 | cwnd（只 hysteria2+bbr-v2） | 补全其他 cc/协议，但当前设备用不到 |
| 3 | 接收窗口 4MB→2MB | 仍崩，崩溃栈换成接收侧 `ParseStreamFrame` 池 |
| 4 | Brutal 拥塞配置 | 查证节点走 BBR 非 Brutal，假设否证 |

**教训（已写入 lessons）**：在能观测之前不要靠改一个旋钮追症状。三次崩溃栈分别指向
三个不同的 quic-go 1452B 池（发送 StreamFrame / 接收 oobConn / 接收 ParseStreamFrame），
说明根因不是任何单个池——是它们共同的某个上游因素。

### 2.2 转折：用 profile 取代猜测

决定性动作是**抓 heap profile**（`go tool pprof`），而不是继续读代码。两份 profile
（早/晚，饱和下载中途）一举定案（见 3.2）。这条也已固化为 lesson：多次假设失败后，
停止猜测，切到 instrumentation 或能执行确切路径的集成测试。

---

## 第三部分：调试工具与方法

### 3.1 真机证据通道（assistant 不能亲跑 speedtest，只能 build/install + 事后分析）

真机 A/B 测试的执行（点测速、看 VPN 掉没掉）是**只有人能做的屏幕操作**。可靠的设备侧证据：

1. **导出 tunnel 日志**：app 内 Settings → Diagnostic → Export Tunnel Log。
   - 崩溃会**没有** `stopTunnel` 行（进程被杀，来不及写）；
   - 中途导出的日志尾部可能被截断；
   - 上传的日志可能**早于**上次重装（检查日志末尾时间戳 vs 重装时间再采信）。
2. **App Group plist 直读**（有线连接时）：
   ```
   devicectl device copy from --domain-type appGroupDataContainer \
     --domain-identifier group.com.playstone.Violet --source Library
   plutil -p Preferences/group.com.playstone.Violet.plist
   ```
   `tunnel_state_json`（Go/NE 写，可能陈旧）vs `widget_vpn_status_raw`（Apple NEVPNStatus，
   新鲜）；**两者不一致时那就是诊断信号**。
3. **Xcode 停止原因串**：调试区顶部 / Issue navigator 给出杀因名（如
   `Terminated due to memory issue` / EXC_RESOURCE limit=50 MB）。栈只显示 goroutine 在哪，
   停止原因串直接点名杀因——**问用户要这个串，别从栈反推**（已固化为 lesson）。

### 3.2 heap profile：读法与陷阱

NE 里内置了 `[GoHeap]` 采样器（Swift 侧每 ~2.5s 读 `phys_footprint`）和 heap profile
写入器（在 footprint 逼近阈值时写 early / late 两份 `violet-heap-*.pprof`）。

读法：
```bash
go tool pprof -top violet-heap-late.pprof            # 看 inuse_space 榜
go tool pprof -base violet-heap-early.pprof \
             violet-heap-late.pprof                  # 早晚 diff，看谁在涨
```

**陷阱**（已固化为 lesson）：`MemProfileRate` 默认 512 KB，单次低频分配会显示为量子处的
一个采样——**只信 MULTI-sample 的站点**，别把单个 512KB 量子读成真实内存。

### 3.3 决定性 profile 数据

```
early: inuse_space 10.6 MB
late:  inuse_space 16.6 MB
  wire.init.0.func1 (StreamFrame 池)   1.14 → 6.64 MB
  sync.Pool.Get 累计                    占 late 堆 75%
```

**活堆峰值只有 16.6 MB，离 50 MB 杀线差得远，进程却在 50 MB 被杀。**

### 3.4 Xcode debug 附着会制造假崩溃（重要）

**Xcode debug 附着时会崩，直接跑不崩。** 调试器（Metal validation、LLDB malloc 记账、
额外映射）给进程叠加几 MB 到十几 MB 的额外 footprint，把它推过 50 MB。

**判断真实内存行为必须用「直接跑 + 导出 tunnel 日志的 footprint-peak」，不能用 Xcode 的
EXC_RESOURCE**——附着会污染出假的 50 MB 崩溃。这是本次排查后期才认清、但极其关键的一点。

### 3.5 验证已落地的通用纪律（均为 lessons）

- **别信 BUILD SUCCEEDED + 源码 diff 就认为改动进了运行的二进制**：`strings` grep 真正运行的
  artifact（`ProxyTunnel.debug.dylib`，64 MB），不是 app 级的 50 KB Mate 桩。
- **grep 含正则元字符的探针用 `grep -F`**：`[CTLink]`、格式串里的 `%.1fMiB` 会被当字符类/
  被切断，造成假阴性。
- **iOS NE 重装不换运行中的 appex**：iOS 缓存运行中的 NE 进程；重装后必须**彻底断开 VPN +
  系统设置里关 VPN 开关**（或删 VPN 配置）才会加载新 appex。确认加载：日志出现
  `[TCP] concurrent proxied-connection ceiling active: 128` 和 `[GoHeap] periodic reclaim`。
- **shell 不加载用户 profile**：gomobile 在 `/Users/lei/go/bin`，需 `export PATH` 或全路径。

---

## 第四部分：根因

崩溃杀因是 **live heap 与 `phys_footprint` 之间的鸿沟**：Go runtime 已释放、但还没还给 OS 的
freed span。

机制链：
1. quic-go 的 StreamFrame 池（`wire/pool.go`）每收一个 STREAM 帧就从无界 `sync.Pool` 取一个
   **`MaxPacketBufferSize = 1452 B`** 的缓冲；高速下载每秒解析上万个包，churn 极高。
2. `sync.Pool` 的 per-P 分片 + 释放的 span 堆积成 MADV-able 但仍映射的页。
3. Go 后台 scavenger 只占 ~1% CPU，追不上这个 churn，**mapped 内存单调上涨**。
4. `phys_footprint`（Jetsam 比对的数）跟着 mapped 涨到 50 MB，而 live heap 一直平在 16 MB。

这解释了为什么 `SetMemoryLimit(40MB)` 从不生效——它比对的是 **live heap**（永远 ~16 MB），
而 iOS 杀的是 **footprint**。

### 4.1 为什么不需要 fork quic-go 改池

本地 `extend/quic-go`（上游 v0.63.0）确认：上游的 `wire/pool.go` 和 metacubex fork 逐字节
相同——**上游也没解决，也是无界 `sync.Pool` + 1452B/帧**。没有可跟进的补丁、没有可配置的池上界。

但追 `receive_stream.go` 得到关键保证：`handleStreamFrameImpl` **先**做流控检查
（`flowController.UpdateHighestReceived`，超接收窗口就报 `FlowControlError` 拆连接），
**通过后**才 `frameQueue.Push` 入重组队列。即 **StreamFrame 池的活跃占用上界 = 接收窗口**
（每流 ≤ MaxStreamReceiveWindow，每连接 ≤ MaxConnectionReceiveWindow）。

所以：活跃缓冲被窗口约束、footprint 缺口由回收器处理——**不需要 fork quic-go**。

---

## 第五部分：修复架构（三层 + gVisor 层）

同一个"压一项、无界项搬家"模式的不同层级：

### 5.1 乘数层——TCP accept 并发信号量
`tunnel/conn_limit.go`。被代理的并发 TCP 连接数本身无界（`HandleTCPConn` 每连接一 goroutine，
`tcpQueue` cap 64 对 TCP 是死代码）。它是所有 per-connection 项（gVisor 缓冲、StreamFrame 池、
cwnd、relay 缓冲）乘上去的那个数。iOS 封 **128 并发**，达上界时新连接**阻塞至多 10s**，超时
关闭该连接（背压不假死；idle 长连接占满槽时不会永久挂起前台新请求）。非 iOS no-op。

### 5.2 归还层——周期 footprint 回收器（核心）
`mate/footprint_reclaim.go`。iOS 上每 **1 秒**检查 mapped bytes，超 **30 MB** 阈值就
`debug.FreeOSMemory()`。在 darwin 上有效是因为 Go 的 `sysUnusedOS` 用 `MADV_FREE_REUSABLE`
（不同于普通 `MADV_FREE`），归还的 span 传播到 `task_info`、真正离开 `phys_footprint`。
门控保证空闲隧道不做无谓 stop-the-world；只在实际回收 ≥1MB 时打日志避免刷屏。
**不牺牲吞吐**（只还已释放内存，不动在途数据）。

### 5.3 尺寸层——QUIC 接收窗口上限
`adapter/outbound/quic_window_ceiling.go`。iOS 封 **6 MB conn / 3 MB stream**，同时钳
**Initial ≤ Max**（tuic/shadowquic/hysteria 预填 Initial=6.4MB 会超过 cap 后的 Max，握手时
击穿保护——这是审核发现的真实缺陷）。覆盖六条 QUIC 路径：hysteria1 / hysteria2 / tuic /
shadowquic（jls-quic-go，内联钳制）/ masque / vless-xhttp。

### 5.4 gVisor 层——ProcessorsPerChannel=1
`listener/sing_tun/server.go`。sing-tun 默认 `max(1, GOMAXPROCS/#FD)`，iOS 上 = 3 个
packet-processor（各一 goroutine + 缓冲）。iOS 固定为 **1**，砍掉 2 个的常驻。加上合并上游
sing-tun 的握手 watcher 优化（`8c8d293`）。上游 MetaCubeX `92433dba` 为内存受限场景做过同款权衡。

---

## 第六部分：速度 vs 内存的边界（实测）

| 接收窗口 | 下载 | footprint 峰值（直接跑） | 余量 |
|---|---|---|---|
| 2 MB / 1 MB | ~53–106 Mbps | ~27 MB | 大，但慢 |
| 4 MB / 2 MB | ~160 Mbps | ~38 MB | 12 MB |
| 6 MB / 3 MB | ~200 Mbps | ~49.7 MB | 0.3 MB（踩线） |

**当前配置：6 MB / 3 MB。** 依据：4 MB 和 6 MB 在 **Xcode debug 附着下都会崩**（§3.4，调试器开销
把两者都推过线），即该 regime 下剩余的崩溃是**调试器诱发、非窗口尺寸决定**的，那就取速度更好的
6MB / 200Mbps。真实 footprint 判断必须用直接跑的 tunnel 日志 footprint-peak。

高速时的断开重连（`[PathMonitor] Network transition detected (interfaceChanged=true)`）是
**Wi-Fi/蜂窝接口切换**触发 route reassert，与内存无关（当时 footprint 33–49 MB 均有），属移动
网络路径抖动。

---

## 第七部分：真机复测协议

1. **直接跑，不要 Xcode debug 附着**（附着制造假的 50 MB 崩溃，§3.4）。
2. 彻底断开 VPN → 系统设置里关 VPN 开关强制重载新 appex → 重连 → 等 5–10 秒稳定。
3. 调试面板 `tcp-window-bytes` 留空（走 512KB 大 ceiling；非空会 CAP 到该值以下）。
4. 一次只改一个变量，测速中不碰面板（每次面板编辑都要 disconnect+reconnect）。
5. 导出 tunnel 日志 + 两份 profile；判断真实内存看 `footprint-peak`，不看 Xcode。
6. 确认新 NE 已加载：日志有 `concurrent proxied-connection ceiling active: 128` +
   `periodic reclaim: mapped …`。

---

## 第八部分：仍未处理 / 遗漏

均**不是**当前崩溃的原因：

1. **statistic map**（`tunnel/statistic/manager.go`）：`connections` 无 cap/TTL，靠 `Leave` 删；
   `Leave` 漏调即永久泄漏。协议无关，值得单独查 Tracker 生命周期。上游有一批
   `fix: close connection after error handling in <outbound>` 在逐个堵同类问题。
2. **NAT 表**（`component/nat/table.go`）：无硬 cap，靠 60s 超时回收，UDP 泛洪期尖峰。
3. **PathMonitor 重连抖动**：app（Swift）侧，接口切换即断速；治法是去抖窗口，与 mihomo 内存
   优化是两件事。
4. **Swift 侧 15s 反应式回收冷却**：已被 §5.2 的 Go 侧 1s 回收器取代，冗余（幂等无害），可清理。
5. **Xcode debug 下的 50MB 崩溃**：调试器开销所致，非产品缺陷；若需在附着下调试，只能靠更小窗口
   +更激进回收，或接受直接跑为准。
6. **非 hysteria2 协议**：封顶代码已在，但仅 hysteria2 真机验证过。

---

## 附录：关键文件索引

| 层 | 文件 | 作用 |
|---|---|---|
| 乘数 | `tunnel/conn_limit.go` | iOS accept 并发信号量 128 + 10s 超时 |
| 归还 | `mate/footprint_reclaim.go` | iOS 周期 FreeOSMemory（1s/30MB） |
| 尺寸 | `adapter/outbound/quic_window_ceiling.go` | 接收窗口 6MB/3MB + Initial≤Max |
| cwnd | `transport/tuic/congestion{,_v2}/cwnd_ceiling.go` | 拥塞窗口 2048 包 |
| 各协议接线 | `hysteria.go` / `hysteria2.go` / `tuic.go` / `shadowquic.go` / `masque.go` / `vless.go` | 调用 ceiling |
| gVisor | `listener/sing_tun/server.go` | ProcessorsPerChannel=1 + TCPWindowBytes clamp |
| runtime | `mate/service.go` | iOS GOMAXPROCS=3 / GCPercent=50 / SetMemoryLimit=40MB / 启动回收器 |
| 仪表 | `mate/runtime_stats.go` / `mate/heap_profile.go` | GoHeap 采样 + profile 写入 |
