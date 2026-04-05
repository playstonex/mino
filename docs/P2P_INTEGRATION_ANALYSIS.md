# Mihomo 工程集成 P2P(直连) 功能可行性分析报告

## 1. 背景与目标
当前 iOS 和 macOS 平台的 Virtual LAN（虚拟局域网）节点通信均通过 Relay（中继）服务器进行转发。为了降低延迟并提升网络体验，目标是在 Virtual LAN 页面实现：用户点击设备节点后，两端设备在可能的情况下建立 P2P（Peer-to-Peer 端到端）直连网络，也就是通常所说的 NAT Traversal（NAT 穿透/打洞）。

## 2. 技术可行性结论
**结论：完全可行。**

`mihomo`（Clash Meta 的分支）是基于 Go 语言开发的代理核心工程。在 Go 语言生态中，实现 P2P 组网和 NAT 穿透的技术库非常成熟。考虑到 `mihomo` 优秀的模块化设计（通过 `adapter`、`transport` 构建网络栈），我们完全可以将 P2P 网络通道封装为 `mihomo` 的底层 Transport 层协议。

## 3. 核心技术原理 (ICE 与 UDP 打洞)
标准的 P2P 穿透基于 ICE (Interactive Connectivity Establishment) 框架：
1. **STUN (发现公网 IP)**：两端设备向 STUN 服务器发起请求，获取各自在 NAT 外侧暴露的公网 IP 和端口。
2. **Signaling (信令交换)**：借助现有的 Relay 中继服务器，将设备 A 的公网 IP 信息发送给设备 B，反之亦然。
3. **Hole Punching (UDP 打洞)**：A 和 B 同时向对方的公网 IP 发送 UDP 探测包。在大部分 NAT 拓扑下，这能在彼此路由器的防火墙外“打出通道”，从而使得对方的数据包能顺利进入内网实现直连。
4. **TURN (中继降级)**：当遇到严格的对称型 NAT（Symmetric NAT）导致打洞失败时，平滑回退至 Relay 中继，保证网络高可用。

## 4. 主流技术选型与推荐
### 方案 A：WebRTC (推荐 - `pion/webrtc`)
* **库地址**：`github.com/pion/webrtc`
* **简介**：优秀的纯 Go 语言 WebRTC 实现。使用其中的 `DataChannel` 可快速构建高可靠 P2P 数据流。
* **优势**：开箱即用，内部原生集成了极其成熟的 ICE 状态机、多次尝试策略以及 STUN/TURN 全套逻辑，极大降低开发成本，成功率极高。
* **结合点**：将 DataChannel 封装进 `net.Conn`，抛给 `mihomo` 核心作为透明加密传输管道。

### 方案 B：libp2p (`go-libp2p`)
* **库地址**：`github.com/libp2p/go-libp2p`
* **简介**：IPFS 团队的核心底层网络框架。
* **优势**：包含 AutoNAT 和强大全面的打洞协议。
* **劣势**：略为沉重，会显著增加产物（如 xcframework）的二进制体积，对轻量的 VPN 拨号需求存在一定冗余。

### 方案 C：自定义 UDP 打洞 (`pion/stun`)
* **库地址**：`github.com/pion/stun` + 自研信令交互
* **简介**：手写原生的 UDP 互发探测逻辑。
* **优势**：极致的轻量级，非常适合构建纯粹的、无多余包头的类 WireGuard 甚至私有协议隧道。
* **劣势**：重难点在于需自己维护复杂的打洞超时、重试、错误回退等网络状态机机制。

## 5. 工程落地架构设计 (Mihomo 视角)
针对当前的 `mihomo` 以及 iOS/macOS 的 `mate` 封装形态，落地步骤建议如下：

1. **拓展协议字典**：在 `mihomo/adapter` 模块下新增 `p2p` 类型 outbound/inbound，使其从配置面被 `mihomo` 所识别。
2. **信令支持**：重用目前的 `violet-server` (Relay 服务器) 暴露 REST 或 WebSocket 接口，作为交换 STUN Session Description 的信令桥梁。
3. **Go 层封装**：如选择 Pion WebRTC，需要在 Go 代码中实例化 PeerConnection，完成 SDP 交换，并在 DataChannel `OnOpen` 回调时，将此通道注入给代理层接管流量。
4. **C-API (Mate) 桥接层**：在 `mihomo/mate` 中暴露 `mate_start_p2p(target_id)` 和观察直连状态的 callback。
5. **Swift 层 UI 联动**：
   * 用户在 SwiftUI 的 Virtual LAN 列表点击设备节点；
   * App 层发起 `mate_start_p2p` 指令；
   * Go 底层静默发起打洞；
   * 回调打洞成功，此时网络自动切换到高优的 P2P 路由，Swift UI 更新状态灯为直连（🟢 P2P）。
