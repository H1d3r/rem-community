# rem

[![CI](https://github.com/chainreactors/rem-community/actions/workflows/test.yml/badge.svg)](https://github.com/chainreactors/rem-community/actions/workflows/test.yml)
[![Go Version](https://img.shields.io/github/go-mod/go-version/chainreactors/rem-community)](https://go.dev/)
[![Release](https://img.shields.io/github/v/release/chainreactors/rem-community)](https://github.com/chainreactors/rem-community/releases)
[![License](https://img.shields.io/github/license/chainreactors/rem-community)](LICENSE)

全场景应用层/传输层代理工具, 支持任意方向、任意信道的隧道与代理。

## 设计理念

rem 中没有 server/client 的概念, 每个节点都是对等的 **Agent**。节点通过 `-s` (listener) 和 `-c` (dialer) 自由组合建立传输层连接, 通过 `-l` 和 `-r` 在连接两端提供应用层服务。

```
                    参数分层

    传输层 (Tunnel)              应用层 (Serve)
    ───────────────              ──────────────
    -s  监听, 等待连入            -l  本端 serve (入口)
    -c  连接到目标节点            -r  对端 serve (出口)

    -s 和 -c 自由组合             -l 和 -r 配对使用
```

```
  ┌─────────────┐                           ┌─────────────┐
  │   Agent A   │◄═══════ Tunnel ═════════►│   Agent B   │
  │             │   tcp / udp / ws / dns    │             │
  │  -l socks5  │   http / http2 / icmp     │  -r raw     │
  │  (入口)     │   wireguard / simplex     │  (出口)     │
  │             │                           │             │
  │   Wrapper: aes / tls / xor / snappy     │             │
  └─────────────┘                           └─────────────┘
```

节点的角色完全由参数组合决定:

| 参数组合 | 角色 |
|---------|------|
| `-s` | 纯监听, 等待 agent 连入 |
| `-c` | 纯连接, 不开 serve |
| `-c -l socks5://...` | 本端开 SOCKS5, 流量从对端出去 |
| `-c -r socks5://...` | 对端开 SOCKS5, 流量从本端出去 |
| `-c -l socks5://... -r socks5://...` | 两端都开 SOCKS5, 互为出口 |
| `-c -s` | 中继节点 (relay): 连上游 + 监听下游 |
| `-c -s -l socks5://...` | 中继 + 本端开 SOCKS5 |

## 核心特性

- **对等组网** — 没有 server/client 之分, `-s`/`-c` 自由组合, 天然支持 relay 和多跳
- **双向 serve** — `-l` 和 `-r` 可同时指定, 两端各自开启 serve, 互为出口
- **多跳路由** — Agent 自动通过 RouteAnnounce 发现拓扑, 支持 10+ 跳链路
- **丰富的传输层** — TCP, UDP, HTTP, HTTP/2, WebSocket, DNS, ICMP, WireGuard, StreamHTTP, Unix Socket
- **Simplex 单工信道** — 通过 DNS 查询、OSS 对象存储、HTTP 轮询等非常规信道建立全双工隧道
- **SR-ARQ 可靠传输** — 为不可靠信道提供选择性重传、滑动窗口、自动分片
- **可插拔加密** — AES, SM4, Twofish, 3DES, CAST5, Blowfish, TEA, XTEA, XOR
- **代理链级联** — 通过 `-x` 串联多层代理, 支持 SOCKS5/HTTP/Shadowsocks 混合链
- **上下行分离** — `up-`/`down-` 前缀将上传和下载拆分到不同信道
- **多连接负载均衡** — ConnHub 管理多条连接, 支持 random / fallback / round-robin
- **自动重连** — 指数退避重连, 断线自动恢复
- **C 库导出** — 编译为 .dll / .so / .a, 支持 C/C++/Python/Rust 等语言调用
- **极小体积** — 支持 TinyGo 编译, 适配嵌入式和受限环境
- **全平台** — Windows, Linux, macOS, ARM, WASM

## 快速开始

```bash
# 1. 最基础: server 监听, client 开 SOCKS5 代理
./rem -s tcp://0.0.0.0:34996
./rem -c tcp://server-ip:34996 -l socks5://127.0.0.1:1080

# 2. 对端开 SOCKS5 (流量从 client 端出去)
./rem -s tcp://0.0.0.0:34996
./rem -c tcp://server-ip:34996 -r socks5://0.0.0.0:2080

# 3. 两端同时开 SOCKS5
./rem -s tcp://0.0.0.0:34996
./rem -c tcp://server-ip:34996 -l socks5://127.0.0.1:1080 -r socks5://0.0.0.0:2080

# 4. 三跳 relay
./rem -s tcp://0.0.0.0:34996                                          # Console
./rem -c tcp://console:34996 -s tcp://0.0.0.0:35000                   # Relay
./rem -c tcp://relay:35000 -l socks5://127.0.0.1:1080                 # Leaf

# 5. 端口转发
./rem -s tcp://0.0.0.0:34996
./rem -c tcp://server-ip:34996 -l port://127.0.0.1:8080 -r raw://target:80

# 6. WebSocket + TLS
./rem -s wss://0.0.0.0:8443/tunnel
./rem -c wss://server-ip:8443/tunnel -l socks5://127.0.0.1:1080

# 7. 自定义加密
./rem -s tcp://0.0.0.0:34996 -k "my-secret"
./rem -c tcp://server-ip:34996 -k "my-secret" -l socks5://127.0.0.1:1080
```

## 组网示例

```
                    Console (:34996)
                   ╱              ╲
         Relay-A (:35000)     Relay-B (:35001)
              │                ╱        ╲
           Leaf-1          Leaf-2     Leaf-3
         socks5:1080     socks5:1081  socks5:1082
```

```bash
# Console
./rem -s tcp://0.0.0.0:34996

# Relay-A: 连 Console, 监听下游
./rem -c tcp://console:34996 -s tcp://0.0.0.0:35000 -a relay-a

# Relay-B: 连 Console, 监听下游
./rem -c tcp://console:34996 -s tcp://0.0.0.0:35001 -a relay-b

# Leaf-1: 连 Relay-A, 开 SOCKS5
./rem -c tcp://relay-a:35000 -l socks5://127.0.0.1:1080

# Leaf-2: 连 Relay-B, 开 SOCKS5
./rem -c tcp://relay-b:35001 -l socks5://127.0.0.1:1081

# Leaf-3: 连 Relay-B, 开 SOCKS5
./rem -c tcp://relay-b:35001 -l socks5://127.0.0.1:1082

# 任意 Leaf 的 SOCKS5 流量穿透整个树到 Console 出去
curl -x socks5://remno1:0onmer@127.0.0.1:1080 http://httpbin.org/ip
```

## 传输层 (Tunnel)

| 协议 | URL Scheme | 说明 |
|------|-----------|------|
| TCP | `tcp://` | 标准 TCP 隧道 |
| UDP | `udp://` | UDP 数据报隧道, 内置 KCP 可靠传输 |
| HTTP | `http://` | HTTP 长轮询隧道 |
| HTTP/2 | `h2://` | HTTP/2 多路复用隧道 |
| StreamHTTP | `streamhttp://` | SSE 下行 + POST 上行, 适合 CDN 场景 |
| WebSocket | `ws://` `wss://` | WebSocket 全双工隧道 |
| DNS | `dns://` | DNS 查询隧道 (需 `-tags dns`) |
| ICMP | `icmp://` | ICMP Echo 隧道 (需 root 权限) |
| WireGuard | `wg://` | WireGuard VPN 隧道 |
| Unix Socket | `unix://` | Unix 域套接字 |
| Memory | `memory://` | 进程内存隧道, 用于 C 库集成 |

### Simplex 单工信道

通过 SR-ARQ 协议将不可靠的单工信道升级为可靠的全双工隧道:

| 信道 | URL Scheme | 说明 |
|------|-----------|------|
| DNS | `simplex+dns://` | DNS TXT/CNAME 记录传输 |
| OSS | `simplex+oss://` | 阿里云 OSS 对象存储传输 (需 `-tags oss`) |
| HTTP | `simplex+http://` | HTTP 轮询传输 |

## 应用层 (Serve)

| 服务 | URL Scheme | 说明 |
|------|-----------|------|
| SOCKS5 | `socks5://` | SOCKS5 代理, 支持认证 |
| HTTP Proxy | `http://` | HTTP/HTTPS 正向代理 |
| Port Forward | `port://` | TCP 端口转发 |
| Raw | `raw://` | 原始 TCP 透传 |
| Shadowsocks | `ss://` | Shadowsocks 代理 |
| Trojan | `trojan://` | Trojan 协议 |
| External C2 | `externalc2://` | 外部 C2 对接 |

## 流量处理 (Wrapper)

| Wrapper | 参数 | 说明 |
|---------|-----|------|
| TLS | `?tls` | TLS 加密 |
| Cryptor | `?wrapper=aes-128-cfb` | 对称加密 |
| Snappy | `?compress` | Snappy 压缩 |
| Padding | `?wrapper=padding` | 流量填充 |

### 加密算法

默认内置: **AES**, **XOR**

通过 build tag 启用: **SM4**, **Twofish**, **3DES**, **CAST5**, **Blowfish**, **TEA**, **XTEA**

## 高级特性

### 代理链级联

通过 `-x` 串联多层出站代理, 通过 `-f` 为 Console 连接指定代理:

```bash
# 出站流量经过两层代理
./rem -c tcp://server:34996 -x socks5://proxy1:1080 -x http://proxy2:8080

# Console 连接走跳板机
./rem -c tcp://server:34996 -f socks5://bastion:1080

# Shadowsocks 出站代理
./rem -c tcp://server:34996 -x ss://aes-256-gcm:password@proxy:8388
```

流量路径:  `本地 → -f 代理链 → Console → Tunnel → -x 代理链 → 目标`

### 上下行分离 (Duplex)

将上传和下载拆分到不同的信道, 适用于不对称网络环境:

```bash
# Server: TCP 上行, UDP 下行
./rem -s up-tcp://0.0.0.0:5555 -s down-udp://0.0.0.0:6666

# Client: 对应的上下行信道
./rem -c up-tcp://server:5555 -c down-udp://server:6666 \
      -l socks5://127.0.0.1:1080
```

`up-` 前缀标记上行信道 (client→server), `down-` 前缀标记下行信道 (server→client)。可混合使用不同协议, 如 TCP 上行 + UDP 下行, 或 HTTP 上行 + WebSocket 下行。

### 多连接负载均衡 (ConnHub)

同时建立多条连接并进行负载均衡:

```bash
# 三条连接, round-robin 轮询
./rem -c tcp://srv1:34996 -c tcp://srv2:34996 -c ws://srv3:8080/tunnel \
      --lb round-robin -l socks5://127.0.0.1:1080

# fallback 模式: 优先使用第一条, 故障时切换
./rem -c tcp://primary:34996 -c udp://backup:34996 --lb fallback
```

| 算法 | 说明 |
|------|------|
| `random` | 随机选择连接 |
| `fallback` | 优先第一条, 故障时降级 |
| `round-robin` | 轮询分发 |

### 多级路由

通过 `-d` 指定目标 Agent, 实现流量在 Agent 网络中跨节点转发:

```bash
# Agent A 作为入口, 将流量路由到 Agent B
./rem -s tcp://0.0.0.0:34996 -d agent-b

# Client 连接, 指定最终目标
./rem -c tcp://gateway:34996 -d "backend" -l socks5://127.0.0.1:1080
```

### 自动重连

断线后自动重连, 支持指数退避:

```bash
# 无限重连, 10s 起始间隔, 最大退避 300s (默认)
./rem -c tcp://server:34996

# 最多重试 10 次, 5s 起始, 120s 上限
./rem -c tcp://server:34996 --retry 10 --retry-interval 5 --retry-max-interval 120

# 也可通过 URL 参数指定
./rem -c "tcp://server:34996?retry=10&retry-interval=5"
```

### C 库集成

编译为共享库后可被其他语言调用:

```bash
# 编译
bash build.sh -buildmode c-shared -o "linux/amd64"
```

```python
# Python 调用示例
from ctypes import cdll, c_char_p
lib = cdll.LoadLibrary('./rem_linux_amd64.so')
lib.InitDialer()
lib.RemDial(c_char_p(b"-c tcp://server:34996 -l socks5://127.0.0.1:1080"))
```

导出函数: `InitDialer()`, `RemDial()`, `MemoryDial()`, `MemoryRead()`, `MemoryWrite()`, `MemoryClose()`, `CleanupAgent()`

## CLI 参考

```
传输层:
  -s, --server <url>            监听地址 (可多次指定)
  -c, --client <url>            连接地址 (可多次指定)
  -a, --alias <name>            Agent 别名
  -d, --destination <id>        目标 Agent ID (多级路由)

应用层:
  -l, --local <url>             本端 serve (可多次指定)
  -r, --remote <url>            对端 serve (可多次指定)

代理链:
  -x, --proxy <url>             出站代理链 (可多次指定, 按顺序级联)
  -f, --forward <url>           Console 连接代理链 (可多次指定)

加密:
  -k, --key <key>               加密密钥

高可用:
  --lb <algo>                   负载均衡: random / fallback / round-robin
  --retry <n>                   最大重连次数 (0=无限)
  --retry-interval <sec>        重连间隔 (默认 10s)
  --retry-max-interval <sec>    最大退避间隔 (默认 300s)

兼容:
  -m, --mod <mode>              兼容旧版: reverse / proxy / bind

其他:
  --list                        列出已注册的组件
  --debug                       调试日志
  -q, --quiet                   静默模式
```

### URL 查询参数

所有 URL 支持通过 query 参数配置:

```
tcp://0.0.0.0:34996?tls&wrapper=aes-128-cfb&retry=5&lb=round-robin&compress
```

## 编译

```bash
# 标准编译 (TCP + SOCKS5)
bash build.sh -a "socks" -t "tcp" -o "linux/amd64"

# 全功能版本
bash build.sh --full -o "linux/amd64,windows/amd64,darwin/arm64"

# 指定传输和应用模块
bash build.sh -a "http,socks,raw,portforward,shadowsocks,trojan" \
              -t "tcp,udp,websocket,http2,streamhttp,dns,icmp" \
              --tags "dns,oss"

# TinyGo 编译 (极小体积)
bash build.sh --tinygo

# C 共享库
bash build.sh -buildmode c-shared -o "linux/amd64"

# C 静态库
bash build.sh -buildmode c-archive -o "linux/amd64"

# 列出已注册组件
./rem --list
```

### 编译期默认值

可在编译时内置默认参数, 生成免配置的一键部署二进制:

```bash
bash build.sh -t "tcp" -a "socks" \
  -s "tcp://your-server:34996" \
  -c "tcp://your-server:34996" \
  -l "socks5://127.0.0.1:1080" \
  -q true
```

## 项目结构

```
rem/
├── cmd/            # CLI 入口与 C 库导出
├── runner/         # Agent 运行时与配置管理
├── agent/          # Agent 核心: 连接桥接、流量路由、组网
├── protocol/
│   ├── core/       # 接口定义与常量
│   ├── tunnel/     # 传输层: tcp, udp, ws, dns, icmp, wg, ...
│   ├── serve/      # 应用层: socks5, http, portforward, ss, trojan
│   ├── wrapper/    # 流量处理: 加密, 压缩, 填充
│   ├── message/    # 协议消息 (wire protocol)
│   └── cio/        # I/O 工具: 缓冲区, 限速器
└── x/              # 扩展库
    ├── arq/        # SR-ARQ 可靠传输协议
    ├── simplex/    # 单工信道传输框架
    ├── yamux/      # 多路复用
    ├── kcp/        # KCP 协议
    ├── cryptor/    # 加密算法注册中心
    ├── encoders/   # 编码器: base64, hex, gzip, ...
    ├── xtls/       # TLS 证书管理
    ├── socks5/     # SOCKS5 实现
    └── trojanx/    # Trojan 协议框架
```

## 文档

https://chainreactors.github.io/wiki/rem/usage/

## 相关项目

| 项目 | 特点 |
|------|------|
| [frp](https://github.com/fatedier/frp) | 最常用的反向代理, 配置丰富但较复杂 |
| [gost](https://github.com/go-gost/gost) | 强大的正向代理, v3 支持反向 |
| [iox](https://github.com/EddieIvan01/iox) | 轻量的端口转发工具 |
| [Xray](https://github.com/XTLS/Xray-core) | 协议隐蔽性与密码学特性优秀 |
| [chisel](https://github.com/jpillora/chisel) | HTTP 隧道工具 |

## License

MIT
