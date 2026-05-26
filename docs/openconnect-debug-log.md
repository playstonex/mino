# OpenConnect 调试记录

> Date: 2026-05-23

## 问题描述

同样的配置使用 AnyConnect 客户端可以正常连接，但 Violet/mihomo 的 OpenConnect 实现无法工作。

## 服务器配置

```yaml
- name: EC2
  type: openconnect
  server: jp.playstone.info
  port: 27443
  username: liang
  password: a
  cisco-compat: true
  skip-cert-verify: true
  no-dtls: false
  remote-dns-resolve: true
  dns:
    - 8.8.8.8
```

## 修复过程

### Bug 1: `ApplyDefaults()` 被跳过

**文件:** `transport/openconnect/tunnel.go`

**问题:** 直接构造 `&base.ClientConfig{...}` 赋值，跳过了 `ApplyDefaults()`，导致：
- `AgentVersion` 为空 — AnyConnect 网关检查 User-Agent 版本号
- `CiscoCompat` 默认 false — 不发送 Cisco 兼容 header

**修复:** 改用 `base.NewClientConfig()` 初始化（自带 `AgentVersion = "4.10.07062"`, `CiscoCompat = true`）。

### Bug 2: TLS ServerName 为空

**文件:** `transport/openconnect/tunnel.go`

**问题:** 自定义 `DialTLS` 中 `tls.Client(rawConn, config)` 的 config 没有 `ServerName`。当 `InsecureSkipVerify = false` 时 TLS 验证会失败。

**修复:** 从 addr 参数提取 host 设置 `config.ServerName`。

### Bug 3: WritePacket buffer 空间不足（关键）

**文件:** `transport/openconnect/tunnel.go`

**问题:** `WritePacket` 创建的 `Payload.Data` 没有预留 CSTP header 的 8 字节空间。`payloadOutTLSToServer` 在发送时做 `pl.Data = pl.Data[:l+8]` 扩展 slice 写入 header，如果底层 array 的 cap 不够会导致数据损坏或 panic。

**表现:** OpenConnect 认证和隧道建立成功，但所有数据传输 `context deadline exceeded`。

**修复:** 分配 `len(data)+8` 的 buffer，确保有空间 prepend CSTP header。

### Bug 4: remote-dns-resolve 无 DNS 服务器

**文件:** `adapter/outbound/openconnect.go`

**问题:** `remote-dns-resolve: true` 但 `dns` 列表为空时，resolver 不会创建，DNS 仍走本地直连被 GFW reset。

**修复:** 当 `Dns` 为空时，自动使用 OpenConnect 服务器在隧道建立时返回的 DNS 服务器。

### 改进: DTLS 感知 + CloseChan 监听

**文件:** `transport/openconnect/tunnel.go`

- `WritePacket` 优先走 DTLS（低延迟），fallback 到 TLS
- `ReadPacket`/`WritePacket` 监听 `cSess.CloseChan`，防止服务端断开后 goroutine 泄漏

## 验证结果

```
[OpenConnect] auth init succeeded, tunnel-group= auth-method=
[OpenConnect] password auth succeeded
[OpenConnect] tunnel established: vpn-ip=10.11.171.130 mask=255.255.0.0 mtu=1333 dns=[1.1.1.1 1.0.0.1 8.8.8.8 8.8.4.4 9.9.9.9 208.67.222.222 208.67.220.220]
[OpenConnect](Ec2) tunnel established: vpn-ip=10.11.171.130/16 mtu=1333
[TCP] 198.18.0.0:50575 --> gspe1-ssl.ls.apple.com:443 match Match using proxy[Ec2]  ✅
[TCP] 198.18.0.0:55613 --> 43.160.156.172:443 match Match using proxy[Ec2]  ✅
[TCP] 198.18.0.0:55621 --> sgshort.wechat.com:80 match Match using proxy[Ec2]  ✅
```

## 已知限制

| 问题 | 影响 | 状态 |
|------|------|------|
| 全局状态不支持并发 | 多个 OpenConnect 节点同时初始化会互相覆盖 | 未修复 |
| DTLS 通道直连不走 mihomo dialer | Network Extension 中可能回环 | 未修复，建议 `no-dtls: true` |
| 速度不如 Hysteria2 | 协议本身限制（TCP vs QUIC） | 预期行为 |

## 性能对比

OpenConnect (CSTP/TCP) vs Hysteria2 (QUIC/UDP):
- OpenConnect 受 TCP 队头阻塞影响
- Hysteria2 多路复用 + 0-RTT + BBR 拥塞控制
- OpenConnect 适合企业兼容性场景，不适合高速代理
