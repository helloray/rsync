# oc-rsync 协议 30+ 技术参考文档

> 本文档基于 oc-rsync Rust 代码库（`_oc-rsync/`）的深入分析，为将 rsync 协议 30+ 增量递归模式移植到 Go 提供精确的线格式和实现参考。

---

## 目录

1. [架构概览](#1-架构概览)
2. [协议握手 (setup_protocol)](#2-协议握手-setup_protocol)
3. [兼容性标志位 (CF_*)](#3-兼容性标志位-cf)
4. [Varint 编码格式](#4-varint-编码格式)
5. [多路复用 I/O](#5-多路复用-io)
6. [文件列表条目线格式](#6-文件列表条目线格式)
7. [增量递归 (INC_RECURSE)](#7-增量递归-inc_recurse)
8. [C rsync 源码参考索引](#8-c-rsync-源码参考索引)
9. [协议陷阱与注意事项](#9-协议陷阱与注意事项)
10. [Go 移植建议](#10-go-移植建议)

---

## 1. 架构概览

### oc-rsync 模块结构

```
crates/protocol/src/
├── compatibility/       # 兼容性标志位定义与协商
│   ├── mod.rs
│   └── flags.rs         # CF_* 标志位常量
├── negotiation/         # 协议版本协商
│   ├── mod.rs
│   ├── types.rs         # ProtocolVersion 类型
│   ├── capabilities/    # 算法能力协商（checksum/compress）
│   └── detector/        # 协议版本检测
├── flist/               # 文件列表编解码
│   ├── read/            # 读取（解码）文件列表条目
│   ├── write/           # 写入（编码）文件列表条目
│   └── incremental/     # 增量递归核心
│       ├── mod.rs       # IncrementalFileList 状态机
│       ├── streaming.rs # 流式文件列表读取
│       └── ready_entry.rs # 就绪条目处理
├── varint/              # Varint/Vstring 编解码
├── multiplex/           # 多路复用 I/O 框架
├── envelope/            # 消息信封（tag + length 编码）
└── wire/                # 基础线格式工具函数
```

### 与 C rsync 的对比

| 方面 | C rsync | oc-rsync |
|------|---------|----------|
| 代码组织 | flist.c 2000+ 行紧耦合 | flist/ 拆分为 10+ 模块 |
| 状态管理 | 大量全局变量 | 清晰的 struct 封装 |
| 增量递归 | `send_file_list` 800 行嵌套 | `IncrementalFileList` 400 行，push/pop 接口 |
| 可读性 | 宏定义多，难以追踪 | Rust 强类型，接口清晰 |
| 测试覆盖 | 依赖外部测试套件 | 模块级单元测试 + 互操作测试 |

---

## 2. 协议握手 (setup_protocol)

### 2.1 版本交换（协议 ≥ 30，二进制模式）

**关键原则**：双方都先写后读，避免 TCP 全双工死锁。

**写入（4 字节）**：
```
byte[0] = protocol_version  (例如 32)
byte[1] = sub_version       (0 表示正式版)
byte[2] = 0                 (保留)
byte[3] = 0                 (保留)
```

**读取（4 字节）**：
```
byte[0] = remote_protocol_version
byte[1..3] = 忽略
```

**协商规则**：`negotiated = min(our_max, remote)`。如果 remote 不在 `[OLDEST=28, NEWEST=32]` 范围内，中止连接。

**参考**：`compat.c:572-644` (setup_protocol)

### 2.2 旧版握手（协议 < 30，daemon 模式）

```
Client → Server: @RSYNCD: <version>.0\n    (例如 "@RSYNCD: 32.0\n")
Server → Client: @RSYNCD: <negotiated>.0\n
```

### 2.3 兼容性标志位交换（协议 ≥ 30）

**方向**：单向 — 服务端写，客户端读。

**服务端写入**：`write_varint(compat_flags as i32)` — varint 编码。

**客户端读取**：`read_varint()` → 转为 `u32` → `CompatibilityFlags`。

**默认标志位**（现代客户端/服务端）：
```
CHECKSUM_SEED_FIX | VARINT_FLIST_FLAGS | SAFE_FILE_LIST |
AVOID_XATTR_OPTIMIZATION | INPLACE_PARTIAL_DIR | ID0_NAMES |
SYMLINK_TIMES (Unix only) | SYMLINK_ICONV (Unix+iconv) |
INC_RECURSE (if allowed)
```

**参考**：`compat.c:710-743`

### 2.4 能力协商（vstring 交换）

仅当双方都有 `CF_VARINT_FLIST_FLAGS` 时发生。

**顺序**（双方同时执行相同操作）：
1. 发送自己的算法列表（vstring）
2. 读取对方的算法列表（vstring）
3. 独立选择第一个双方都支持的算法

**Vstring 编码**（注意：不是 varint！）：
```
len ≤ 0x7F:  1 字节 (len) + payload
len > 0x7F:  2 字节 (len/0x100 + 0x80, len & 0xFF) + payload
最大 len = 0x7FFF
```

**参考**：`compat.c:534-585` (negotiate_the_strings), `io.c:2297-2315`

### 2.5 Checksum Seed 交换

**所有协议版本**。服务端写，客户端读。

**写入**：4 字节 LE（i32 seed）。`seed = time() ^ (pid << 6)` 或固定值。

**参考**：`options.c:847`

### 2.6 握手完整时序图

```
Server                          Client
  |                               |
  |--- write version (4B) ------->|  (先写)
  |--- write compat_flags (var) ->|
  |--- write checksum_seed (4B) ->|
  |                               |
  |<---- read version (4B) -------|  (后读)
  |<---- read compat_flags (var) -|
  |<---- read checksum_seed (4B) -|
  |                               |
  |  [能力协商 - 如果 CF_VARINT_FLIST_FLAGS]
  |<=== vstring (algorithms) ====>|  (同时读写)
  |                               |
  |  [进入多路复用 I/O 阶段]
  |                               |
```

---

## 3. 兼容性标志位 (CF_*)

以 varint 编码在线上传输。位定义：

| 位 | 标志名 | Rust 常量 | C 常量 | 说明 |
|----|--------|-----------|--------|------|
| 0 | INC_RECURSE | `INC_RECURSE` | CF_INC_RECURSE | 增量递归支持 |
| 1 | SYMLINK_TIMES | `SYMLINK_TIMES` | CF_SYMLINK_TIMES | 符号链接时间戳 |
| 2 | SYMLINK_ICONV | `SYMLINK_ICONV` | CF_SYMLINK_ICONV | 符号链接 iconv 转换 |
| 3 | SAFE_FILE_LIST | `SAFE_FILE_LIST` | CF_SAFE_FILE_LIST | 安全文件列表模式 |
| 4 | AVOID_XATTR_OPTIMIZATION | `AVOID_XATTR_OPTIMIZATION` | CF_AVOID_XATTR_OPTIM | 禁用 xattr 优化 |
| 5 | CHECKSUM_SEED_FIX | `CHECKSUM_SEED_FIX` | CF_CHKSUM_SEED_FIX | 校验和种子修复 |
| 6 | INPLACE_PARTIAL_DIR | `INPLACE_PARTIAL_DIR` | CF_INPLACE_PARTIAL_DIR | inplace + partial dir |
| 7 | VARINT_FLIST_FLAGS | `VARINT_FLIST_FLAGS` | CF_VARINT_FLIST_FLAGS | varint 编码文件列表标志 |
| 8 | ID0_NAMES | `ID0_NAMES` | CF_ID0_NAMES | id0 名称支持 |
| 25 | CONSECUTIVE_MATCH | `CONSECUTIVE_MATCH` | (oc-only) | oc-rsync 私有扩展 |

**参考**：`crates/protocol/src/compatibility/flags.rs`, `compat.c:117`

---

## 4. Varint 编码格式

### 4.1 varint (i32)

| 首字节模式 | 额外字节数 | 总字节数 | 数值范围 |
|-----------|-----------|---------|---------|
| `0xxx_xxxx` (0x00-0x7F) | 0 | 1 | 0..127 |
| `10xx_xxxx` (0x80-0xBF) | 1 | 2 | 0..16383 |
| `110x_xxxx` (0xC0-0xCF) | 2 | 3 | 0..2097151 |
| `1110_xxxx` (0xD0-0xD3) | 3 | 4 | 0..268435455 |
| `1111_0xxx` (0xD4-0xD7) | 4 | 5 | 任意 i32 |

**查找表** `INT_BYTE_EXTRA[64]`，按 `first_byte / 4` 索引：
- 0x00-0x1F → 0 额外字节
- 0x20-0x2F → 1 额外字节
- 0x30-0x33 → 2 额外字节
- 0x34-0x34 → 3 额外字节
- 0x35-0x35 → 4 额外字节
- 0x36-0x37 → 5（溢出错误）

**解码**：读取首字节，查表获取额外字节数，读取对应字节数，从小端字节重建 i32。

**编码**：找到所需的最小字节数，在首字节设置标记位，按小端写入。

### 4.2 varlong (i64, min_bytes)

与 varint 原理相同，但用于 64 位值，带 `min_bytes` 参数（时间/大小通常为 4）。

### 4.3 longint（协议 < 30）

- 值 0..0x7FFFFFFF：4 字节 LE (i32)
- 更大：0xFFFFFFFF (4 字节) + 8 字节 LE (i64)

### 4.4 vstring（不是 varint！）

- len ≤ 0x7F：1 字节长度 + payload
- len > 0x7F：2 字节 `(len/0x100+0x80, len&0xFF)` + payload
- 最大 len = 0x7FFF

**参考**：`crates/protocol/src/varint/`, `io.c:read_varint()`, `io.c:2297-2315`

---

## 5. 多路复用 I/O

握手完成后，所有数据都带有 4 字节头。

### 5.1 帧头（4 字节 LE）

```
byte[0..2] = payload_length (24 位 LE，最大 0x00FFFFFF)
byte[3]    = tag = MPLEX_BASE + message_code
```

其中 `MPLEX_BASE = 7`，`PAYLOAD_MASK = 0x00FFFFFF`。

**解码**：
```go
tag := (raw_u32 >> 24) & 0xFF
code := tag - MPLEX_BASE   // MPLEX_BASE = 7
length := raw_u32 & 0x00FFFFFF
```

### 5.2 消息类型代码

| 代码 | 名称 | 说明 |
|------|------|------|
| 0 | Data | 常规数据负载 |
| 1 | ErrorXfer | 传输错误 |
| 2 | Info/FLUSH | 信息消息 / 刷新信号 |
| 3 | Error | 一般错误 |
| 4 | Warning | 警告消息 |
| 5 | ErrorSocket | 套接字错误 |
| 6 | Log | 日志消息 |
| 7 | Client | 客户端消息 |
| 8 | ErrorUtf8 | UTF-8 错误 |
| 9 | Redo | 重做请求 |
| 10 | Stats | 统计信息 |
| 22 | IoError | I/O 错误 |
| 33 | IoTimeout | I/O 超时 |
| 42 | NoOp | 心跳保活 |
| 86 | ErrorExit | 错误退出 |
| 100 | Success | 成功 |
| 101 | Deleted | 已删除 |
| 102 | NoSend | 不发送 |

**空 DATA 帧**（len=0）：心跳保活。读取器必须静默吸收 — 不能返回 EOF。

**参考**：`crates/protocol/src/envelope/`, `io.c:965 send_msg()`, `io.c:680-716`

---

## 6. 文件列表条目线格式

### 6.1 条目解码顺序 (recv_file_entry)

每个条目的线上传输顺序：

1. **Flags** — varint（协议 ≥ 30 + CF_VARINT_FLIST_FLAGS）或单字节
2. **Name** — 前缀压缩（见下文）
3. **Hardlink index** — 如果设置了 `XMIT_HLINKED`，varint 索引
4. **File size** — varlong(4) 或 longint（协议 < 30）
5. **Mtime** — varlong(4)，如果不是 `XMIT_SAME_TIME`
6. **Nsec** — varint，如果设置了 `XMIT_MOD_NSEC`，范围 [0, 999999999]
7. **Crtime** — varlong(4)，如果保留且不是 `XMIT_CRTIME_EQ_MTIME`
8. **Mode** — 4 字节 LE u32，如果不是 `XMIT_SAME_MODE`（通过 `from_wire_mode` 转换）
9. **Atime** — varlong(4)，如果保留、非目录、不是 `XMIT_SAME_ATIME`
10. **UID** — varint（或 4 字节 LE，协议 < 30），如果不是 `XMIT_SAME_UID`
11. **User name** — u8 长度 + 字节，如果设置了 `XMIT_USER_NAME_FOLLOWS`（协议 30+，仅 inc_recurse）
12. **GID** — varint/4 字节，如果不是 `XMIT_SAME_GID`
13. **Group name** — u8 长度 + 字节，如果设置了 `XMIT_GROUP_NAME_FOLLOWS`
14. **Rdev major/minor** — 如果是设备/特殊文件
15. **Symlink target** — vstring，如果是符号链接
16. **Hardlink dev+ino** — 如果 `XMIT_HLINKED` 且不是缩写跟随者
17. **Checksum** — 如果 `--checksum` 模式

### 6.2 主标志位（首字节，位 0-7）

| 位 | 标志 | 含义 |
|----|------|------|
| 0 | XMIT_TOP_DIR | 顶层目录 |
| 1 | XMIT_SAME_MODE | 模式与上一条目相同 |
| 2 | XMIT_EXTENDED_FLAGS | 扩展标志位跟随 |
| 3 | XMIT_SAME_UID | UID 与上一条目相同 |
| 4 | XMIT_SAME_GID | GID 与上一条目相同 |
| 5 | XMIT_SAME_NAME | 名称前缀与上一条目相同 |
| 6 | XMIT_LONG_NAME | 名称后缀使用 codec（非 u8） |
| 7 | XMIT_SAME_TIME | 修改时间与上一条目相同 |

### 6.3 扩展标志位（第二字节，位 8-15）

| 位 | 标志 | 含义 |
|----|------|------|
| 0 | XMIT_SAME_RDEV_MAJOR / XMIT_NO_CONTENT_DIR | 共享位 |
| 1 | XMIT_HLINKED | 条目有硬链接信息 |
| 2 | XMIT_SAME_DEV_PRE30 / XMIT_USER_NAME_FOLLOWS | 协议相关 |
| 3 | XMIT_RDEV_MINOR_8_PRE30 / XMIT_GROUP_NAME_FOLLOWS | 协议相关 |
| 4 | XMIT_HLINK_FIRST / XMIT_IO_ERROR_ENDLIST | 共享位 |
| 5 | XMIT_MOD_NSEC | 纳秒字段跟随 |
| 6 | XMIT_SAME_ATIME | 访问时间相同 |
| 7 | XMIT_UNUSED_15 | 未使用 |

### 6.4 第三字节（位 16-23，仅 varint 模式）

| 位 | 标志 | 含义 |
|----|------|------|
| 0 | XMIT_RESERVED_16 | 保留 |
| 1 | XMIT_CRTIME_EQ_MTIME | 创建时间等于修改时间 |

### 6.5 名称前缀压缩

- 如果 `XMIT_SAME_NAME`：读取 u8 `same_len`（与前一条目共享的字节数）
- 如果 `XMIT_LONG_NAME`：通过 codec 读取 suffix_len（varint）
- 否则：读取 u8 `suffix_len`
- 总长度 = same_len + suffix_len，必须 < MAXPATHLEN (4096)
- 名称 = prev_name[0..same_len] + new_suffix

### 6.6 列表结束标记

- 非 varint：单零字节 (0x00)
- varint：零标志 varint，然后另一个 varint 表示 io_error（0 = 正常结束）

### 6.7 I/O 错误标记（安全文件列表模式）

在安全文件列表模式下，标志 `XMIT_EXTENDED_FLAGS | (XMIT_IO_ERROR_ENDLIST << 8)` 是错误哨兵，后跟 varint 错误码。

**参考**：`crates/protocol/src/flist/read/mod.rs`, `flags.rs`, `name.rs`, `metadata.rs`, C: `flist.c:recv_file_entry()` 760-1050

---

## 7. 增量递归 (INC_RECURSE)

### 7.1 线协议

初始文件列表之后，发送方按目录流式传输子列表：

1. 发送方写**子列表头**：`write_ndx(NDX_FLIST_OFFSET - dir_ndx)`，其中 `dir_ndx` 是父目录的线索引
2. 发送方写该目录内容的文件条目（与初始列表相同的线格式）
3. 发送方写列表结束标记
4. 对下一个目录重复
5. 发送方写 `NDX_FLIST_EOF` (-2) 终止所有子列表

**关键常量**：
- `NDX_FLIST_OFFSET = -101` — 子列表头标记基数
- `NDX_FLIST_EOF = -2` — 所有文件列表结束
- `NDX_DONE` — 传输完成信号

### 7.2 子列表帧格式

每个子列表头值：`ndx = NDX_FLIST_OFFSET - dir_ndx`
- 如果 `ndx > NDX_FLIST_OFFSET`：协议违规
- 如果 `ndx == NDX_FLIST_EOF`：全部完成
- 否则：`dir_ndx = NDX_FLIST_OFFSET - ndx` 标识父目录

### 7.3 NDX 编号

NDX 值在所有段中是顺序的：
- 初始列表：NDX 0..N-1
- 第一个子列表：NDX N..N+M-1
- `seg_ndx_start = prev_ndx_start + prev_used + 1`
- `+1` 是目录条目本身的计数

### 7.4 缩写硬链接跟随者

leader 在**同一段**的跟随者（`hardlink_idx >= seg_ndx_start`）是"缩写的"：
- 线上：只写 flags + name + hardlink_idx；**没有元数据**
- 接收方从当前段的 leader 条目复制元数据
- 更新压缩状态以匹配发送方的 statics

leader 在**前一段**的跟随者是"非缩写的"：
- 完整的元数据在线上
- 接收方正常读取

### 7.5 压缩状态持久性

压缩状态（prev_name, prev_mode, prev_uid, prev_gid, prev_mtime, prev_atime）跨子列表持久化。缓存的 `FileListReader` 在每个段中重用。

### 7.6 ID 列表

- 无 INC_RECURSE：UID/GID id-lists 在初始文件列表之后
- 有 INC_RECURSE：ID 列表在所有子列表之后发送（NDX_FLIST_EOF 之后）

### 7.7 依赖跟踪

`IncrementalFileList` 维护：
- `ready: VecDeque<FileEntry>` — 父目录存在的条目
- `pending: HashMap<String, Vec<FileEntry>>` — 等待父目录的条目
- `created_dirs: HashSet<String>` — 已产出的目录

根目录 `""` 和 `"."` 隐式在 `created_dirs` 中。

当目录条目被 push 时：
1. 如果父目录在 created_dirs → 加入 ready 队列，将自身加入 created_dirs，递归释放待处理的子条目
2. 否则 → 在 pending map 中以父目录为键添加

**参考**：`crates/protocol/src/flist/incremental/mod.rs`, `crates/transfer/src/receiver/file_list/incremental.rs`, C: `flist.c:recv_file_list()`, `io.c:read_a_msg()`

---

## 8. C rsync 源码参考索引

| 主题 | C 源文件 | 行号 |
|------|---------|------|
| CF_* 宏定义 | `compat.c` | 117 |
| setup_protocol() | `compat.c` | 572-644 |
| check_sub_protocol() | `compat.c` | 133-160 |
| negotiate_the_strings() | `compat.c` | 534-585 |
| compat_flags 交换 | `compat.c` | 710-743 |
| read_varint() | `io.c` | （varint 读取） |
| write_varint() | `io.c` | （varint 写入） |
| write_vstring() | `io.c` | 2297-2315 |
| send_msg() | `io.c` | 965 |
| perform_io() | `io.c` | 680-716 |
| read_a_msg() | `io.c` | （增量段读取） |
| set_io_timeout() | `io.c` | 1148-1157 |
| recv_file_entry() | `flist.c` | 760-1050 |
| send_file_entry() | `flist.c` | 470-750 |
| recv_file_list() | `flist.c` | 2887+ |
| flist_sort_and_clean() | `flist.c` | 3016-3082 |
| send_extra_file_list() | `flist.c` | 3073 |
| 名称前缀压缩 | `flist.c` | 843-883 |
| clean_fname() | `util1.c` | 943 |
| Seed 生成 | `options.c` | 847 |
| NDX 常量 | `rsync.h` | 286-288 |

---

## 9. 协议陷阱与注意事项

### 9.1 Vstring ≠ Varint

vstring 长度前缀编码与 varint 不同。对 vstring 长度使用 varint 会在长度 > 0x7F 时静默破坏线格式。

### 9.2 空 DATA 心跳

空 DATA 帧（payload_len=0 的 4 字节头）是心跳。读取器必须静默吸收 — 返回 `Ok(0)` 会被解释为 EOF。

### 9.3 MPLEX_BASE 偏移

多路复用头中的 tag 字节是 `MPLEX_BASE + code`，其中 `MPLEX_BASE = 7`。忘记这个偏移会错误解码所有消息类型。

### 9.4 缩写跟随者压缩状态

处理缩写硬链接跟随者时，必须从 leader 更新压缩状态（prev_mode, prev_mtime, prev_uid, prev_gid, prev_atime），即使没有从线上读取元数据。发送方在 `goto the_end` 之前更新了其 statics，所以接收方必须镜像此操作以保持后续条目的同步。

### 9.5 Name Follows 标志仅在 INC_RECURSE 下

`XMIT_USER_NAME_FOLLOWS` / `XMIT_GROUP_NAME_FOLLOWS` 仅在 `inc_recurse && user_name` 时设置。没有 `inc_recurse` 时，名称在尾部的 id-list 中传输。在没有 inc_recurse 时内联发送它们会与上游偏离。

### 9.6 NDX 段边界

`seg_ndx_start = prev_ndx_start + prev_used + 1`。`+1` 至关重要 — 它考虑了触发子列表的目录条目。

### 9.7 IO 错误标记检查顺序

I/O 错误标记检查（`XMIT_EXTENDED_FLAGS | (XMIT_IO_ERROR_ENDLIST << 8)`）必须在提取扩展字节之后、将标志解释为文件条目标志之前进行。仅在安全文件列表模式下有效。

### 9.8 Varint 模式列表结束

在 varint 模式下，零标志 varint 后面跟着另一个携带 io_error 代码的 varint。在非 varint 模式下，零字节就是列表结束。

### 9.9 协议 < 30 IO 错误尾随

协议 < 30 在 id-lists 之后发送 4 字节 LE i32 io_error。协议 ≥ 30 使用 MSG_IO_ERROR 或 SAFE_FILE_LIST 标记。

### 9.10 排序先去重

`flist_sort_and_clean()` 先排序再去重。接收方始终运行去重（将丢弃的条目标记为 tombstone 以保持 NDX 对齐）。恶意发送方发出重复名称必须折叠为单一条目。

### 9.11 子列表路径验证

子列表中的每个条目必须位于其头 `dir_ndx` 命名的目录下。不匹配是协议违规（恶意发送方注入路径）。

### 9.12 XXHash 的校验和种子

握手期间交换的校验和种子用于 XXHash 算法。MD5 不使用它。种子是 `time() ^ (pid << 6)`。

### 9.13 压缩状态固定大小缓冲区

`prev_name` 是固定 `[u8; 4096]` 数组（MAXPATHLEN），不是 Vec。名称前缀压缩长度限制为 255（单字节）。

### 9.14 硬链接 GNUM 分配

Leader GNUM (F_HL_GNUM) 是 readdir 顺序的线 NDX，在排序之前分配。在接收方，`hlink_first` leader 在读取后但排序之前将其 `hardlink_idx` 设置为 `seg_ndx_start + i`。

### 9.15 Iconv 和未排序的 Flist

当 `--iconv` 激活时，文件列表保持线（NDX）顺序而不进行原地排序。只有单独的排序指针数组被重新排序。这保持了 NDX 寻址与发送方意图一致。

---

## 10. Go 移植建议

### 10.1 推荐的 Go 包结构

```
internal/
├── protocol/
│   ├── handshake.go      # setup_protocol 实现
│   ├── compat.go         # CompatibilityFlags 定义
│   ├── varint.go         # varint/vstring 编解码
│   ├── multiplex.go      # 多路复用 I/O 读写器
│   └── envelope.go       # 消息信封（tag + length）
├── flist/
│   ├── entry.go          # FileEntry 结构体
│   ├── reader.go         # 文件列表条目读取（解码）
│   ├── writer.go         # 文件列表条目写入（编码）
│   ├── flags.go          # XMIT_* 标志位常量
│   └── incremental.go    # IncrementalFileList 状态机
└── rsyncd/
    └── rsyncd.go         # daemon 主循环
```

### 10.2 关键数据结构

```go
// CompatibilityFlags 兼容性标志位
type CompatibilityFlags uint32

const (
    CFIncRecursive       CompatibilityFlags = 1 << 0
    CFSymlinkTimes       CompatibilityFlags = 1 << 1
    CFSymlinkIconv       CompatibilityFlags = 1 << 2
    CFSafeFileList       CompatibilityFlags = 1 << 3
    CFAvoidXattrOptim    CompatibilityFlags = 1 << 4
    CFChecksumSeedFix    CompatibilityFlags = 1 << 5
    CFInplacePartialDir  CompatibilityFlags = 1 << 6
    CFVarintFlistFlags   CompatibilityFlags = 1 << 7
    CFID0Names           CompatibilityFlags = 1 << 8
)

// FileEntry 文件条目
type FileEntry struct {
    Name      string    // 相对路径
    DirName   string    // 父目录（interned）
    Size      uint64
    ModTime   int64
    Mode      uint16    // POSIX mode (type + perms)
    UID       uint32
    GID       uint32
    ModNsec   uint32
    
    // 可选字段
    SymlinkTarget string
    RdevMajor     uint32
    RdevMinor     uint32
    HardlinkIdx   int32  // -1 如果不是硬链接
    
    // 存在性标记
    HasUID      bool
    HasGID      bool
    IsTopDir    bool
    IsContentDir bool
    IsHlinkFirst bool
}

// IncrementalFileList 增量文件列表状态机
type IncrementalFileList struct {
    ready    []*FileEntry           // 已就绪条目
    pending  map[string][]*FileEntry // 等待父目录的条目
    createdDirs map[string]bool     // 已创建的目录
    incRecursive bool               // 是否增量模式
}

func (l *IncrementalFileList) Push(entry *FileEntry)
func (l *IncrementalFileList) Pop() *FileEntry
func (l *IncrementalFileList) MarkDirCreated(path string)
```

### 10.3 实施顺序建议

1. **Phase 1: 基础协议（1 周）**
   - 实现 `varint.go`：varint/vstring 编解码
   - 实现 `handshake.go`：setup_protocol（版本协商 + compat_flags + seed）
   - 实现 `multiplex.go`：多路复用 I/O 读写器
   - 测试：确保与 C rsync 3.x 握手成功

2. **Phase 2: 文件列表（1 周）**
   - 实现 `flags.go`：XMIT_* 常量
   - 实现 `reader.go`：文件列表条目解码
   - 实现 `writer.go`：文件列表条目编码
   - 实现 `entry.go`：FileEntry 结构体
   - 测试：与 C rsync 交换文件列表

3. **Phase 3: 增量递归（2 周）**
   - 实现 `incremental.go`：IncrementalFileList 状态机
   - 修改 `reader.go`/`writer.go`：支持子列表和 NDX_FLIST_EOF
   - 实现依赖跟踪逻辑
   - 测试：与 C rsync 增量递归模式互操作

4. **Phase 4: 集成测试（1 周）**
   - 与 C rsync 3.x 双向测试（push/pull）
   - 大目录树测试（10 万+ 文件）验证内存占用
   - 互操作测试：Go sender → C receiver, C sender → Go receiver

### 10.4 调试建议

- 使用 Wireshark 或自定义代理捕获线格式数据
- 打印每条消息的 tag、length、payload 前几个字节
- 与 C rsync 的 `rsync -vvvv` 输出对比
- 先实现只读（接收文件列表），确保能解析 C rsync 发送的数据
- 再实现只写（发送文件列表），确保 C rsync 能解析

### 10.5 关键测试用例

```go
// TestVarintEncoding 测试 varint 编解码
func TestVarintEncoding(t *testing.T) {
    cases := []int32{0, 127, 128, 16383, 16384, 2097151, -1}
    for _, v := range cases {
        encoded := EncodeVarint(v)
        decoded, err := DecodeVarint(encoded)
        if decoded != v {
            t.Errorf("varint roundtrip failed: %d -> %d", v, decoded)
        }
    }
}

// TestCompatibilityFlags 测试标志位序列化
func TestCompatibilityFlags(t *testing.T) {
    flags := CFIncRecursive | CFVarintFlistFlags | CFSafeFileList
    encoded := EncodeVarint(int32(flags))
    // 验证线格式字节
    expected := []byte{0x89} // bit 0 + bit 3 + bit 7 = 1 + 8 + 128 = 137 = 0x89
    if !bytes.Equal(encoded, expected) {
        t.Errorf("flags encoding mismatch")
    }
}

// TestMultiplexFrame 测试多路复用帧
func TestMultiplexFrame(t *testing.T) {
    // DATA 帧，payload = "hello"
    frame := EncodeFrame(0, []byte("hello")) // code=0, MPLEX_BASE=7
    // 预期：length=5, tag=7 → 4字节头 + 5字节payload
    expected := []byte{0x05, 0x00, 0x00, 0x07, 'h', 'e', 'l', 'l', 'o'}
    if !bytes.Equal(frame, expected) {
        t.Errorf("frame encoding mismatch")
    }
}
```

---

## 附录 A：快速参考卡片

### 常量速查

```go
// 协议版本
const (
    ProtocolOldest  = 28
    ProtocolNewest  = 32
    MPLEX_BASE      = 7
    PayloadMask     = 0x00FFFFFF
)

// NDX 常量
const (
    NDxFlistOffset = -101
    NDxFlistEOF    = -2
    NDXDone        = -1
)

// XMIT 标志位（主字节）
const (
    XmitTopDir         = 1 << 0
    XmitSameMode       = 1 << 1
    XmitExtendedFlags  = 1 << 2
    XmitSameUID        = 1 << 3
    XmitSameGID        = 1 << 4
    XmitSameName       = 1 << 5
    XmitLongName       = 1 << 6
    XmitSameTime       = 1 << 7
)

// XMIT 扩展标志位（第二字节）
const (
    XmitSameRdevMajor  = 1 << 8  // 或 XmitNoContentDir
    XmitHlinked        = 1 << 9
    XmitSameDevPre30   = 1 << 10 // 或 XmitUserNameFollows
    XmitRdevMinor8Pre30 = 1 << 11 // 或 XmitGroupNameFollows
    XmitHlinkFirst     = 1 << 12 // 或 XmitIoErrorEndlist
    XmitModNsec        = 1 << 13
    XmitSameAtime      = 1 << 14
)
```

### 握手时序

```
1. 双方写版本 (4B)
2. 双方读版本 (4B)
3. 协商: min(our, remote)
4. Server 写 compat_flags (varint)
5. Client 读 compat_flags
6. Server 写 checksum_seed (4B LE)
7. Client 读 checksum_seed
8. [如果 CF_VARINT_FLIST_FLAGS] 能力协商 (vstring)
9. 进入多路复用 I/O
```

---

*文档生成时间：2026-08-28*
*基于 oc-rsync v0.6.4 代码库分析*
