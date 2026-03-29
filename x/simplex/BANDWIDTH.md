# cloud storage Transport 带宽计算

## 单个 Item 容量

cloud storage List 的多行文本列 (allowMultipleLines=true) 通过 Graph API 写入时，
字段值上限约 **255 KB**。数据经 base64 编码后存入该字段。

### 编码链路

```
原始数据 (raw bytes)
  ↓ TLV 序列化 (+5 bytes/packet 头)
  ↓ base64 标准编码 (×4/3 膨胀)
  ↓ JSON 字段值 (+ ~80 bytes JSON 外壳: {"fields":{"col":"...","Timestamp":"...","UID":"..."}})
  → cloud storage Graph API POST /items
```

### 精确计算

```
cloud storage 列值上限 C = 261,120 bytes  (255 × 1024)
JSON 外壳开销      J ≈ 100 bytes       (字段名 + 时间戳 + UID + JSON 标点)
可用 base64 空间   B = C - J = 261,020 bytes

base64 编码: 每 3 字节原始 → 4 字节 base64
  raw_max = floor(B / 4) × 3
          = floor(261020 / 4) × 3
          = 65255 × 3
          = 195,765 bytes

当前常量: Maxcloud storageMessageSize = 190,000 (保守值, 浪费 ~3%)
建议调整: Maxcloud storageMessageSize = 195,000 (更贴近上限, 留 ~0.4% 安全余量)
```

### 每个 Item 实际有效载荷

```
每个 SimplexPacket 有 5 字节 TLV 头 (Type 1B + Length 4B)。
一个 Item 中打包 N 个 packet:

有效载荷 = raw_max - N × 5

典型情况 (ARQ segment 经 SimplexPacket 封装):
  ARQ MSS = 1024:  packet = 1029B, 每 item ≈ 189 个 packet, 有效 ≈ 194,055B
  ARQ MSS = 65535: packet = 65540B, 每 item ≈ 2 个 packet,  有效 ≈ 194,990B

当只有 1 个大 packet 时: 有效载荷 ≈ raw_max - 5
```

## 多 Item 传输

一次 poll cycle (tick) 可发送最多 `maxItemsPerSendCycle` 个 Item (当前=8)。
每个 Item 独立写入同一个 cloud storage List，接收端逐个 poll 读取。

### 吞吐量公式

```
单向吞吐量 = min(N, ceil(total / raw_max)) × raw_max / interval

其中:
  N         = maxItemsPerSendCycle (每 tick 最多发的 item 数)
  raw_max   = Maxcloud storageMessageSize (单 item 原始字节上限)
  interval  = polling 间隔 (秒)
  total     = 待发送总字节数
```

### 吞吐量表 (理论值, KB/s)

```
            │ 1 item/tick │ 4 items/tick │ 8 items/tick
────────────┼─────────────┼──────────────┼──────────────
 5s (默认)  │    38.1     │    152.3     │    304.7
 3s         │    63.5     │    253.9     │    507.8
 1s         │   190.4     │    761.7     │   1523.4
 500ms      │   380.9     │   1523.4     │   3046.9
```

### Item 拆分示例

```
以 raw_max = 195,000 为例:

  数据量      │ 需要 items │ 末尾 item 填充率
─────────────┼────────────┼─────────────────
  64 KB      │     1      │  33.6%
 190 KB      │     1      │  99.7%  ← 几乎凑满
 195 KB      │     1      │ 100.0%  ← 刚好凑满
 256 KB      │     2      │  34.3%  (第2个 item 只装 67KB)
 512 KB      │     3      │  64.1%
   1 MB      │     6      │  42.0%
```

## 关键常量

| 常量 | 值 | 说明 |
|------|-----|------|
| Maxcloud storageMessageSize | 195000 | 单 item base64 前最大 raw 字节 |
| maxItemsPerSendCycle | 8 | 每 tick 最多发送的 item 数 |
| Defaultcloud storageInterval | 5000 | 默认 poll 间隔 (ms) |
| TLV header | 5 | 每个 SimplexPacket 的头部开销 |
| base64 膨胀率 | ×1.333 | 标准 base64 (每 3B → 4B) |

## 优化建议

1. **凑满 item**: `appendFromQueueLocked` 已按 `maxBodySize` 打包,
   将 `Maxcloud storageMessageSize` 从 190000 调至 195000 可减少不必要的 item 拆分。

2. **多 item/tick**: `maxItemsPerSendCycle=8` 允许队列积压时一次 tick 发完,
   避免大传输被拉长 N 倍 interval。

3. **interval 调优**: 降低 interval 线性提升吞吐, 但增加 API 请求频率,
   需权衡 Graph API throttling (通常 > 100 req/min 开始限流)。
