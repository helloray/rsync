# 接收端内存优化:逐条目释放文件列表

状态:**已实施**(2026-09-11),含逐条目释放 + 重同步跳过释放;单元测试、双向 C 互操作与内存测量见文末。

## 背景

用户主要把 Go daemon 当**接收端**使用(C rsync 客户端推送大源码树)。实测 10 万文件级别的树,daemon 峰值 RSS ~120 MB;百万文件会到 ~1 GB。

发送端已经过 lazy send1extra 重构(见 `docs/sendextra.md`),峰值从 ~100 MB 降到 ~17 MB(100k 文件);但那次优化只覆盖 sender。接收端在 inc-recurse 下**把收到的每个文件条目保留到传输结束**,是当前内存的主要来源。

## 龙去脉

### C rsync 的对照

C 接收端同样在 inc-recurse 下保留 flist 条目到传输结束(删除对比、权限修复都需要),但每条目只有 ~150-250 B(flist struct + 文件名)。C 的百万文件场景也要几百 MB——"文件列表吃内存"是 rsync 公认痛点,rsync 3.4.x 没有改变这一点。

| | 每条目开销 | 10 万文件 | 100 万文件 |
|---|---|---|---|
| C rsync | ~150-250 B | ~20-30 MB | ~200-300 MB |
| Go (本 fork) | ~800 B-1 KB | ~80-120 MB | ~1 GB |

Go 的开销构成:File 结构体(多个 string 字段)+ `byNdx` map 槽位(每条目一项,数据帧按 ndx 路由用)+ GC 余量(RSS 通常为存活堆的 ~1.5-2 倍)。差 3-5 倍常数,**量级与 C 相同**。

### 为什么当前实现"接收一部分释放一部分"做不到

一个条目的生命周期很长,且有 4 个消费者:

```
段到达 → 进队列 → 生成器处理(建目录/发请求) → 数据帧异步到达(按 ndx 路由) → 文件写完
                                                                    ↓ 最后
                          删除对比(--delete) + touchUpDirs(目录权限修复)
```

- 数据帧是异步的:发送端有自己的在途窗口(可同时传上千个文件),条目必须从"段到达"活到"它的数据写完"才能释放;
- `--delete` 的删除对比需要完整文件名列表(与磁盘目录树做差集);
- touchUpDirs 在传输结束后还要遍历所有目录条目。

当前实现为了正确性,把 `allFiles`、`byNdx`、`dirs` 全部保留到最后。

### 关键结论:问题不是"何时释放",而是"释放后还剩什么"

真正难的不是判断释放时机,而是把"必须留到最后的数据"降级成最小表示:

## 优化方案(评估结论)

### 有把握且安全的部分(核心收益)

- `byNdx` 的唯一写者是帧循环 goroutine(无锁竞争)。某个 ndx 的数据帧接收完成(`recvFile1` 返回)后,该条目再也不会被用到 → `delete(byNdx[N])`,File 结构体可被 GC。这是单 goroutine 内的操作,零并发风险。仅此一项就消灭主要开销(~700 B/条目)。
- 属性-only 的 itemize 回显(无数据传输的 ndx)也会按 ndx 查表 → 查到空槽走现有 `continue` 分支即可。
  实施时的安全性依据(C 源码核对):发送端每个文件只写**一条** ndx+iflags 记录(sender.c:766),数据传完后不再回显;dry-run 的无数据回显(sender.c:638-641)与请求同帧被消费,因此"收完即释放"在普通传输与 dry-run 下都成立。

### 必须保留的部分(用"降级"而非"释放")

- 删除对比:只需**文件名列表** → 保留紧凑的 `[]string`(~100 B/条),完整 File 结构体照常释放;
- touchUpDirs:只需**目录条目**(数量 = 目录数,本来就便宜);
- 段释放(`freeOne`)与 sender 的 DONE 协议不动。

### 预期收益

峰值 ≈ 在途窗口(~1000 条)+ 目录数 + 删除用名字列表:10 万文件 ~120 MB → ~30-40 MB;百万文件 ~1 GB → ~300 MB,**量级上追平 C**。名字列表是硬下限,这是"降常数"不是"降量级"。

### 风险与对策

1. **生命周期判断错误 → 路由 panic / 死锁**(本 fork 曾在 sender 调度器上栽过同类跟头:commit f79095d,预算计数漂移导致 >1000 条目的树挂死)。本方案的风险评级更低:释放点 = 帧循环收到该文件完整数据的时刻,是协议上的强保证,**没有需要跨 goroutine 维护的计数器**,不存在 f79095d 那类漂移。
2. **`--delete` / dry-run / 中断恢复的交互** → 用现有 C 互操作矩阵覆盖,特别是带 `--delete` 的双向同步与错误中断路径。
3. 实现规模:核心改动一两百行;主要成本在测试覆盖(delete + resume + 错误路径组合)。

### 工作量与时机

评级:中低,比 send1extra 重构小(不需要跨 goroutine 的精确计数)。**建议等真实场景功能问题全部跑通后再做**;触发条件:实际同步的树达到几十万文件以上、内存预算紧张时再启动。

## 实施记录(2026-09-11)

按上述方案实施,并补上了原方案遗漏的**重同步场景**:

1. **数据收完即释放**(`incremental.go` `releaseFile`):帧循环 `recvFile1` 返回后 `delete(byNdx, ndx)`。
2. **names 降级**(`pushEntries`/`deleteList`):`allFiles` 移除;`names []string` 仅在 `--delete` 时累积(`deleteList` 只被 `deleteIncWhenReady` 消费,非 `--delete` 传输不保留,百万文件级可省数十 MB);`dirs []*File` 保留给 touchUpDirs。完整 File 结构体随释放 GC。
3. **跳过即释放**(超出原方案的补充):原方案只在"收到数据"时释放,但 daemon 的主要工作模式是**重复推送同一棵树**——重同步时没有变化文件、没有数据帧,`byNdx` 会照旧累积全部条目,收益归零。补充:生成器处理完条目且未写传输请求(`genRequested == false`,即跳过/仅属性变更)时,把 ndx 通过**非阻塞 channel**(`queueSkipRelease`)交给帧循环统一 `delete`(`applySkipReleases`)。`byNdx` 仍然只有帧循环一个写者,零锁竞争;队列满时丢弃释放只影响内存、不影响正确性。跳过的条目不可能再有帧:Go 生成器不为跳过输出 itemize(发送端无从回显),也没有请求对应的数据帧;dry-run 请求(`genRequested == true`)不释放,回显照常查表。

正确性由三层测试保证:`internal/receiver/incremental_test.go`(释放/跳过释放语义)、`integration/interop/deeptree_test.go`(双向 C↔Go,含 `--delete`、dry-run、重同步)、`integration/interop/receiver_memory_test.go`(54k 文件实测 + 重同步回归)。

### 测量(C rsync 3.4.4 → Go daemon,122,265 文件,Windows,WorkingSet64 200ms 采样)

| 场景 | 优化前 | 优化后 |
|---|---|---|
| 首次全量推送(峰值 RSS) | 58.5 MiB | 26.2 MiB |
| 重推送/无变化(峰值 RSS) | 56.4 MiB | 29.4 MiB |
| 进程内 54k 文件传输期间最大存活堆(强制 GC 后采样) | 10.2 MB(重同步 10.7 MB) | 3.3 MB(重同步 3.8 MB) |

A/B 同树同场景对比(二进制分别构建于 stash 前后):首推 58.5 → 26.2 MiB(-55%);重推送验证跳过释放——优化前与首推持平(56.4 MiB,条目全部滞留),优化后与首推同量级(29.4 MiB)。历史记录的"10 万文件 ~120 MB"在本测量环境未复现(可能为不同树形/时期的观测),以上同条件 A/B 为准。

`TestReceiverMemoryLargeTree` 断言传输期间最大存活堆 ≤ 8 MB:实测新旧 3.3-3.8 MB / 10.2-10.7 MB,阈值两侧均有 ≥2 倍余量。注意该测试需显式 `SetProtocolVersion(32)` 强制增量递归:Go 客户端构造 server 选项串用的是协商前的默认版本(27),不携带 `-e.<flags>` 能力块,C 发送端会退回完整文件列表(见下方遗留问题)。

### 遗留问题(本任务范围外)

Go 客户端(`rsyncclient`)在协商前构造 `ServerCommandOptions`,`Options.protocol_version` 仍是 gokrazy 默认的 27,导致 `serveroptions.go` 的 `ProtocolVersion() >= 30` 门不通过,`-e.iLfxCvIu` 能力块从不发出:与 C 服务器经 remote-shell 通信时**永远退回完整文件列表**(daemon 路径不受影响,它协商后再构造)。修复需把 argstr 的版本判断改为客户端自身最高支持版本(对齐 C:options.c 在 spawn server 前即知 max protocol),并补齐双向互操作回归。

## 相关文件

- `internal/receiver/incremental.go` — incRecv(byNdx/allFiles/dirs/queue 的持有者)
- `internal/receiver/generator.go` — recvGenerator / touchUpDirs
- `internal/receiver/receiver.go` / `receiverrenameio.go` — 数据接收与落盘
- `internal/receiver/do.go` — 删除对比(deleteFiles / deleteList)
- `docs/sendextra.md` — 发送端 lazy 化的先例(收益与验证方法可参照)
