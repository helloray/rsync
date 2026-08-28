# Rsync 协议 30+ 开发启动指南

> 新会话启动时阅读此文件，快速恢复上下文。

---

## 项目背景

将 gokrazy/rsync（Go 实现）从协议 27 升级到协议 30+，支持增量递归模式（incremental recursion），降低大目录树同步的内存占用。

### 目标

- 协议版本：28-32（兼容 C rsync 3.x）
- 核心功能：增量递归（`--inc-recursive`）
- 内存目标：92 万文件从 ~1GB 降至 O(当前目录文件)

### 当前状态

- **已完成**：协议 27 基本功能（delta 传输、daemon 模式、认证）
- **待实现**：协议 30+ 握手、varint 编码、增量递归

---

## 关键参考文件

| 文件 | 说明 |
|------|------|
| `docs/oc-rsync-protocol-reference.md` | **必读** — 完整协议参考，线格式、标志位、陷阱 |
| `D:/filebrowser-official/_oc-rsync/` | oc-rsync Rust 源码，架构清晰，优先参考 |
| `D:/filebrowser-official/_c-rsync/` | C rsync 源码，仅用于对比验证 |

---

## 实施路线图

### Phase 1: 基础协议（1 周）

**目标**：与 C rsync 3.x 完成握手，进入多路复用 I/O 阶段。

**任务**：
1. `internal/protocol/varint.go` — varint/vstring 编解码
2. `internal/protocol/handshake.go` — setup_protocol 实现
3. `internal/protocol/multiplex.go` — 多路复用 I/O 读写器
4. `internal/protocol/compat.go` — CompatibilityFlags 定义

**验证**：
```bash
# 启动 Go daemon
./gokr-rsyncd --port=8732 --config=rsyncd.conf

# C rsync 客户端连接
rsync -vv rsync://localhost:8732/test/
# 应该能看到成功的握手日志
```

### Phase 2: 文件列表（1 周）

**目标**：能解析 C rsync 发送的文件列表，也能发送文件列表给 C rsync。

**任务**：
1. `internal/flist/flags.go` — XMIT_* 标志位常量
2. `internal/flist/entry.go` — FileEntry 结构体
3. `internal/flist/reader.go` — 文件列表条目解码
4. `internal/flist/writer.go` — 文件列表条目编码

**验证**：
```bash
# Go receiver 接收 C sender 的文件列表
rsync -vv /local/dir/ rsync://localhost:8732/test/
# Go daemon 应该能正确打印接收到的文件名和元数据
```

### Phase 3: 增量递归（2 周）

**目标**：实现 `--inc-recursive`，子列表流式传输。

**任务**：
1. `internal/flist/incremental.go` — IncrementalFileList 状态机
2. 修改 `reader.go`/`writer.go` — 支持子列表和 NDX_FLIST_EOF
3. 实现依赖跟踪（parent-child 关系）
4. 处理硬链接缩写跟随者

**验证**：
```bash
# 增量递归模式
rsync -vv --inc-recursive /large/tree/ rsync://localhost:8732/test/
# 应该看到 "sending incremental file list" 而非 "building file list"
```

### Phase 4: 集成测试（1 周）

**任务**：
- 与 C rsync 3.x 双向测试（push/pull）
- 大目录树测试（10 万+ 文件）验证内存占用
- 互操作测试：Go sender → C receiver, C sender → Go receiver

---

## 开发原则

### 1. 优先参考 oc-rsync，而非 C rsync

**原因**：
- oc-rsync 是 Rust 实现，架构清晰，模块化好
- C rsync 代码太老（1990 年代风格），全局状态多，难以追踪
- oc-rsync 的注释会标注对应的 C rsync 行号（如 `flist.c:760`）

**方法**：
- 先读 oc-rsync 的 `crates/protocol/src/flist/incremental/` 理解增量递归
- 再读 `crates/protocol/src/compatibility/flags.rs` 理解标志位
- 需要对比验证时，才去看 C rsync 的对应行

### 2. 先实现只读，再实现只写

**原因**：
- 读取（解析）比写入（生成）容易调试
- 可以先用 C rsync 作为发送方，验证解析逻辑
- 写入时需要处理更多边界情况（前缀压缩、标志位优化等）

### 3. 每个 Phase 都做互操作测试

**原因**：
- 协议错误会在后期才暴露，调试成本高
- 早期发现线格式问题，修复成本低

**方法**：
- 用 Wireshark 或自定义代理抓包
- 打印每条消息的 tag、length、payload 前几字节
- 与 C rsync 的 `rsync -vvvv` 输出对比

---

## 常见陷阱（详见协议参考文档 §9）

1. **Vstring ≠ Varint**：vstring 长度前缀编码与 varint 不同
2. **MPLEX_BASE 偏移**：tag = MPLEX_BASE + code，MPLEX_BASE = 7
3. **空 DATA 心跳**：len=0 的 DATA 帧必须静默吸收，不能返回 EOF
4. **缩写硬链接跟随者**：必须更新压缩状态，即使没从线上读元数据
5. **NDX 段边界**：`seg_ndx_start = prev_ndx_start + prev_used + 1`，+1 不能忘

---

## 调试工具

### 1. 代理抓包

```go
// proxy.go — 简单的 TCP 代理，打印收发的字节
// 编译：go build -o proxy proxy.go
// 使用：./proxy --listen=8736 --target=8733 (MSYS2 daemon)
```

### 2. C rsync 详细日志

```bash
# 客户端详细日志
rsync -vvvvv /source/ rsync://localhost:8732/test/

# Daemon 日志（在 rsyncd.conf 中）
log file = /tmp/rsyncd.log
transfer logging = yes
log format = %o %h [%i] %l %f
```

### 3. Go daemon 调试日志

在关键位置添加日志：
```go
log.Printf("[handshake] wrote version: %d", protocolVersion)
log.Printf("[handshake] read remote version: %d", remoteVersion)
log.Printf("[flist] received entry: name=%s, mode=%o, size=%d", entry.Name, entry.Mode, entry.Size)
```

---

## 测试环境

### 已安装的工具

- **MSYS2 rsync**：`D:\tools\msys64\usr\bin\rsync.exe`（协议 32，rsync 3.4.4）
- **cwRsync**：`D:\tools\cwrsync\bin\rsync.exe`（协议 30，rsync 3.0.7）
- **Go 1.21+**：编译 Go daemon

### 测试目录结构

```
D:\rsync-test\
├── source\          # 测试源目录
│   ├── file1.txt
│   ├── subdir\
│   │   └── file2.txt
│   └── ...
└── dest\            # 测试目标目录
```

### rsyncd.conf 示例

```ini
[test]
    path = D:/rsync-test/dest
    read only = no
    write only = no
    list = yes
```

---

## 快速恢复命令

```bash
# 停止旧的 daemon 进程
Get-Process gokr-rsyncd | Stop-Process -Force

# 编译 Go daemon
cd D:\filebrowser-official\_rsync-fork
go build -o gokr-rsyncd ./cmd/gokr-rsyncd/

# 启动 daemon
./gokr-rsyncd --port=8732 --config=D:\rsync-test\rsyncd.conf

# 测试连接
rsync -vv rsync://localhost:8732/test/
```

---

## 会话结束时的检查清单

在结束每个开发会话前，确保：

- [ ] 代码能编译通过
- [ ] 基本的互操作测试通过（至少握手成功）
- [ ] 没有遗留的 daemon 进程占用端口
- [ ] 更新了此文档的"当前状态"部分

---

*最后更新：2026-08-28*
