# iOS Network Extension 内存杀进程排查与修复

本文档记录 Violet iOS Network Extension（Mate xcframework）在高速下载时被系统
`EXC_RESOURCE (RESOURCE_TYPE_MEMORY, limit=50 MB)` 杀死的完整排查过程、根因，以及最终
落地的三项修复。结论：**iPhone 12 / 日本 hysteria2 节点，下载 160 Mbps，footprint 峰值
38 MB，无 SIGKILL**。

相关提交：`d7ea6607`、`509c613a`、`04d2ddb4`。

---

## 一、症状

- 真机（iPhone 12，iOS 26.6）跑测速时，Network Extension 被
  `Terminated due to memory issue`（SIGKILL 9，`EXC_RESOURCE` limit=50 MB）杀死。
- 表现为双峰：下载**要么 100+ Mbps 然后崩溃**，**要么塌到 <1 Mbps**，中间没有稳定档。
- 上传始终正常（~30 Mbps）。

iOS 给 `NEProvider` 类扩展的整个进程约 50 MB 的 `phys_footprint` 预算，Go runtime、
用户态 gVisor TCP 栈、Swift/ObjC runtime、所有线程栈都算在内。

---

## 二、排查中被否证的假设

排查经历多轮，每一轮都符合同一个陷阱——**压住一个 per-connection 尺寸项，无界的东西就
搬到下一处**：

| 轮次 | 假设 / 修改 | 结果 |
|---|---|---|
| 1 | gVisor TCP 窗口 / cwnd 太大 | MTU 2048 把 gVisor chunk 池 12→2.85MB，榜首换成 quic-go StreamFrame 池 |
| 2 | cwnd 上界只装在 hysteria2+bbr-v2 一条路径 | 补全 cubic/reno/v1 + tuic/shadowquic/masque（`509c613a`），但当前设备用不到 |
| 3 | 接收窗口 4MB 太大 | 降到 2MB/1MB，仍崩——崩溃栈换成接收侧 `ParseStreamFrame` 池 |
| 4 | Brutal 拥塞配置 | 查证节点未设 `up`/`down`，走的是 BBR，非 Brutal，假设否证 |

关键教训：**在能观测之前不要靠改一个旋钮去追症状**。前四轮都是在读代码猜"是哪个池"，
每次猜中的池都不同（发送 StreamFrame、接收 oobConn 缓冲、接收 ParseStreamFrame），
说明根因不是任何单个池。

---

## 三、根因：footprint 与 live heap 的归还缺口

真正的突破来自两份 heap profile（`violet-heap-early/late.pprof`，饱和下载中途抓取）：

```
early: inuse_space 10.6 MB
late:  inuse_space 16.6 MB
  wire.init.0.func1 (StreamFrame 池)  1.14 → 6.64 MB
  sync.Pool.Get 累计                   占 late 堆 75%
```

**活着的 Go 堆峰值只有 16.6 MB，离 50 MB 杀线差得远，进程却在 50 MB 被杀。**

杀因是 **live heap 与 `phys_footprint` 之间的鸿沟**：Go runtime 已释放、但还没还给 OS 的
freed span。机制：

1. quic-go 的 StreamFrame 池每秒 Get/Put 上万次（每个包一个 1452B 池化缓冲），churn 极高。
2. `sync.Pool` 的 per-P 分片 + 释放的 span 堆积成 MADV-able 但仍映射的页。
3. Go 后台 scavenger 只占 ~1% CPU，追不上这个 churn，mapped 内存单调上涨。
4. `phys_footprint`（Jetsam 比对的数）跟着 mapped 涨到 50 MB，而 live heap 一直平的。

这也解释了为什么 `SetMemoryLimit(40MB)` 从不生效——它比对的是 live heap（永远 ~16 MB），
而 iOS 杀的是 footprint。

### Xcode 调试器放大了假象

后续确认：**Xcode debug 附着时会崩，直接跑不崩**。调试器（Metal validation、LLDB malloc
记账、额外映射）给进程叠加了额外 footprint，把它推过 50 MB。因此**判断真实内存行为必须
直接跑 + 导出 tunnel 日志的 `footprint-peak`，不能用 Xcode 附着**——附着会制造假的
`EXC_RESOURCE`。

---

## 四、修复

三项修复对应同一个"压一项、无界项搬家"模式的三个层级。

### 4.1 TCP accept 并发信号量 — 封住乘数

`tunnel/conn_limit.go`

被代理的并发 TCP 连接数**本身无界**（`HandleTCPConn` 每连接一 goroutine，`tcpQueue`
cap 64 对 TCP 是死代码，无准入控制）。它是所有 per-connection 项（gVisor 缓冲、StreamFrame
池、cwnd、relay 缓冲）乘上去的那个数。

修复：iOS 上用一个容量 128 的信号量封顶并发连接。达到上界时新连接**阻塞等待空位**（背压，
不丢弃）——对饱和传输就是让发送方 pace 到可用槽位，对正常浏览完全不触及。非 iOS 平台是
no-op，行为逐字节不变。

### 4.2 周期 footprint 回收器 — 关闭归还缺口（核心修复）

`mate/footprint_reclaim.go`

profile 证明的病根是"span 不归还"，不是"池对象太大"。修复：iOS 上每 **1 秒**检查 mapped
bytes，超过 **30 MB** 阈值就调 `debug.FreeOSMemory()`。

它在 darwin 上有效，是因为 Go 的 `sysUnusedOS` 用 `MADV_FREE_REUSABLE`（不同于普通
`MADV_FREE`），归还的 span 会传播到 `task_info`，真正离开 `phys_footprint`。这正是软内存
限制拉不动的那根杠杆。门控（阈值 + 间隔）保证空闲隧道不做无谓的 stop-the-world，只在真的
在累积未归还 span 时才回收。**不牺牲吞吐**——只归还已释放内存，不动在途数据；实测 GC CPU
0–0.5%，stop-the-world 有充足余量。

代码库原注释把这个 helper 定义为"给决定要崩的调用者，不给 timer"——profile 就是那个决定，
由数据做出：每次饱和传输 footprint 都在冲杀线，gated timer 正是赶在 Jetsam 之前接住它。

### 4.3 接收窗口上限 6MB/3MB — 给速度

`adapter/outbound/quic_window_ceiling.go`

接收窗口按目标吞吐 × RTT 的 BDP 定：实测 RTT ~460 ms，100 Mbps 需要 ~5.75 MB BDP，故设
6 MB conn / 3 MB stream。回收器兜住这个更大窗口带来的 churn，窗口负责交付速度——两者配合
才同时拿到速度和不崩。

---

## 五、结果

真机（iPhone 12，日本 hysteria2 节点，直接跑非 Xcode 附着）：

- **下载 160 Mbps**
- **footprint 峰值 38 MB**（离 50 MB 杀线 12 MB 余量）
- **无 SIGKILL、无 `PRESSURE`**

日志里出现 `[TCP] concurrent proxied-connection ceiling active: 128` 与
`[GoHeap] periodic reclaim: mapped …` 证明新 NE 已加载、回收器在把 span 压回去。

测速中途速度回落是 `[PathMonitor] Network transition detected (interfaceChanged=true)`
——Wi-Fi/蜂窝接口切换触发 route reassert，**与内存无关**（当时 footprint 才 33–38 MB）。

---

## 六、代码审核发现与修复（follow-up）

一轮逐行代码审核暴露了 `509c613a`/`04d2ddb4` 里的若干不对称与遗漏，已全部修复（见 follow-up 提交）：

- **接收窗口 Initial>Max 倒挂（严重，已修）**：ceiling 原先只 cap `Max*`，不碰 `Initial*`。
  tuic/shadowquic/hysteria 预填 `Initial = Default/10 = 6.4MB`，cap 后 `Max = 6MB` →
  Initial 6.4MB > Max 6MB，握手时向对端宣告的初始窗口直接击穿了保护上限。现在 ceiling 同时
  把 Initial 钳到 Max（含 deliberate-break 回归测试）。
- **Hysteria v1 遗漏 ceiling（严重，已修）**：`adapter/outbound/hysteria.go` 从不调用
  `applyPlatformQUICWindowCeiling`，64MB 默认连接窗口在切 hysteria v1 节点时直接打穿 iOS 预算。
  已补上调用。
- **128 信号量无超时（中，已修）**：slot 占用整个连接生命周期，iOS 大量 idle 长连接（推送、
  心跳、后台 keep-alive）占满 128 槽后，新的前台连接会无限阻塞（表现为"突然卡住"）。acquire
  现在最多等 10s，超时返回 ok=false，`handleTCPConn` 关闭该连接而非阻塞或为其发起 outbound。
- **回收器冗余读 + 日志刷屏（低，已修）**：去掉门控后多余的第二次 `mappedBytes()`；日志改为
  仅在实际回收 ≥1MB 时打印，避免持续大流量下 1Hz 刷屏。
- **测试未 Pin-by-Value（低，已修）**：ceiling 断言改为对字面值 6MB/3MB 断言（经 `wantConnCeiling`/
  `wantStreamCeiling` 常量），并加独立的 pin-check——把常量误改回 64MB 现在会测试失败而非虚假绿灯。

二次审核补充（同样已修）：

- **shadowquic Initial>Max 倒挂（严重，已修）**：shadowquic 用 jls-quic-go 的 `*Config` 类型，
  无法调共享的 `applyPlatformQUICWindowCeiling`，其内联 cap 块只 cap 了 Max、漏了 Initial，
  同样的 6.4MB>6MB 倒挂。已在内联块补上 Initial≤Max 钳制。
- **VLESS xhttp（HTTP/3）默认窗口穿透（中，已确诊并修）**：`transport/xhttp/client.go` 的
  `QUICConfig` 不设接收窗口（0 值），`common.DialQuic` 把未修改的 cfg 交给 quic-go，后者套用
  15MB 连接 / 6MB 流的默认值，绕过 6MB/3MB 保护。已在 `adapter/outbound/vless.go` 两个 xhttp
  QUIC 回调入口加 `applyPlatformQUICWindowCeiling(cfg)`。
- **Initial 倒挂回归测试加强**：`TestQUICWindowCeilingClampsInitialToMax` 原先 stream Initial 设
  1.5MB（本就低于 3MB，没真正测到 stream clamp 分支），已改为 6.4MB，让连接级和流级两个 Initial
  倒挂分支都被触发。

至此 iOS 上 hysteria1/hysteria2/tuic/shadowquic/masque/vless-xhttp 六条 QUIC 路径均受 6MB/3MB
硬边界约束，无协议可绕过。

## 七、仍未处理 / 遗漏

以下均**不是**当前崩溃的原因，按优先级记录：

1. **statistic map**（`tunnel/statistic/manager.go`）：`connections` map 无 cap/TTL，
   靠 `Leave` 删除。`Leave` 漏调的路径会永久泄漏。协议无关，值得单独查 Tracker 生命周期。
2. **NAT 表**（`component/nat/table.go`）：`mapping` 无硬 cap，靠 60s 超时回收，
   UDP 泛洪期尖峰。
3. **PathMonitor 重连抖动**：app（Swift）侧，接口切换即断速；治法是加去抖窗口，与本文的
   mihomo 内存优化是两件事。
4. **Swift 侧 15s 反应式回收冷却**：已被 §四的 Go 侧 1s 回收器取代，现为冗余（无害，
   两边都调 `FreeOSMemory`，幂等），可清理。
5. **非 hysteria2 协议**（ss/trojan/tuic/masque/hysteria v1）：封顶代码已在，但仅
   hysteria2 真机验证过——切协议时才生效。

---

## 八、复测协议（真机）

1. **直接跑，不要 Xcode debug 附着**（附着会制造假的 50 MB 崩溃）。
2. 彻底断开 VPN → 系统设置里关 VPN 开关强制重载新 appex → 重连 → 等 5–10 秒稳定。
3. 调试面板 `tcp-window-bytes` 留空。
4. 一次只改一个变量，测速中不碰面板。
5. 用 app 内 Settings→Diagnostic→Export Tunnel Log 导出日志；判断真实内存看
   `footprint-peak`，不看 Xcode。
