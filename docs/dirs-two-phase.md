# 接收端目录列表两阶段化 + 哈希化(方案C+)

状态:**已实施**(2026-09-14)。本文档是实施前的设计定稿,依据是对当前代码全部消费点的逐处核对(见"现状与消费点分析")。实测(80k 目录树,Go client push → Go daemon):daemon 峰值 RSS 37.2 → 19.1 MB,gctrace 最大存活堆 23 → 6 MB;目录空间索引 24 B/目录(哈希)+ 按需收集的 retouch 集合。

前置工作(已实施,见 `docs/receiver-memory.md`):数据收完即释放、重同步跳过释放、names 仅在 `--delete` 时累积、daemon 连接结束 `FreeOSMemory`。完成后接收端剩余的全量驻留只有一项:**目录条目列表 `inc.dirs []*File`**,它保留完整 `File` 结构直到传输结束——这就是本方案要消化的最后一块。

## 背景

`File` 结构约 120 B(flist.go:59:Name/Length/ModTime/Mode/Uid/Gid/LinkTarget/Rdev*/Checksum[16]/Ndx),加上名字字符串与 GC 开销,每目录实际 ~150-200 B。OpenWrt SDK 树 82,780 个目录 ≈ **13-16 MB** 全程驻留;百万目录级树(构建产物、内核源码)会到 ~150-200 MB。

C rsync 的 `dir_flist` 是指向 flist 条目的指针数组(每目录仅 8 B,条目本身反正全量保留),所以 C 为目录付的额外成本可忽略;但 C 的全量 flist(150-250 B/条目,含目录)保留到结束,总量远高于本 fork。方案C+ 做完后,Go 接收端的全量驻留将**低于 C 的目录索引本身**。

## 现状与消费点分析

`inc.dirs`(incremental.go:50)按到达顺序排列 = 发送端的 dir index 空间。全量 grep 后确认消费者只有两处:

### 消费者 1:段校验(整个传输期间,热路径)

`pushEntries`(incremental.go:108-155)每收到一个文件列表段,用段头里的 `dirIdx` 取父目录名做校验:

```go
if int(dirIdx) >= len(inc.dirs) {
    return nil, fmt.Errorf("invalid file-list dir index %d (have %d dirs)", dirIdx, len(inc.dirs))
}
dirName = inc.dirs[dirIdx].Name
...
if dirIdx >= 0 && flist.ParentPath(fe.Name) != dirName {
    return nil, fmt.Errorf("file-list entry %q does not belong to dir %q", fe.Name, dirName)
}
```

关键观察:只需要 **`Name` 字段**,而且只需要"能证明相等",不需要名字本身。这是哈希化的依据。

### 消费者 2:touchUpDirs(传输结束后,一次性)

`Transfer.Do`(do.go:179-189)在 errgroup join 之后:

```go
fileList = rt.inc.dirs
if rt.retouchDirPerms {
    if err := rt.touchUpDirs(fileList); err != nil { ... }
}
```

`touchUpDirs`(generator.go:46-66)遍历全部目录,但逐条过滤:

- 非目录 / DryRun → 跳过(dirs 里本来就全是目录);
- `mode&S_IWUSR > 0`(可写)→ 跳过,**不需要 touch-up**;
- 其余(不可写目录)→ `setPerms`,需要 Name/Mode/ModTime/Uid/Gid。

且"是否不可写"在生成器建目录时已经判过一次——generator.go:186-191 检测到 `S_IWUSR == 0` 时置 `rt.retouchDirPerms = true` 并临时加上写位。也就是说,**不可写目录集合在建目录那一刻就能精确收集**,无需等到最后遍历全量。

### 并发纪律(现状,必须保持)

- `dirs` 帧循环 goroutine 独占写(incremental.go:42-49 注释);
- `retouchDirPerms` 生成器 goroutine 独占写;
- touchUpDirs 在 `eg.Wait()` 之后才读,与两个写者天然同步(happens-before 由 errgroup 建立)。

## 设计

把 `dirs []*File` 拆成两个紧凑结构,完整 `File` 在生成器处理完该目录后即可 GC(byNdx 槽位已由跳过释放清掉,dirs 是最后的锚点):

### 1. 段校验:`dirHashes [][2]uint64`

`dirs []*File` → `dirHashes [][2]uint64`(每目录固定 24 B:两个 64-bit 哈希 + 切片槽位),pushEntries 时写入:

```go
if f.isDir() {
    inc.dirHashes = append(inc.dirHashes, dirHash128(f.Name))
}
```

校验改为哈希比较:

```go
if int(dirIdx) >= len(inc.dirHashes) { ... 报错同现在 ... }
want := inc.dirHashes[dirIdx]
if dirIdx >= 0 && dirHash128(flist.ParentPath(fe.Name)) != want {
    return nil, fmt.Errorf("file-list entry %q does not belong to dir index %d", fe.Name, dirIdx)
}
```

哈希函数:128-bit(如 maphash.Seed 固定化的 FNV-1a 双 pass 或 xxh128),要求快(每条目每段一次)、确定性(daemon 重启后对同一名单稳定——不涉及持久化,其实无需跨进程稳定,但确定性便于调试)。**碰撞概率**:10⁶ 目录时 ~10⁻²⁷,可忽略;错误方向是"放过一个坏段",且坏段后续还会被正常的协议流挡住。

校验强度评估:parent-name 精确匹配 → 128-bit 哈希匹配。防的目标是协议错位/发送端 bug 导致的段错挂,哈希同样能拦;区别仅在理论碰撞,可接受。

错误信息从"does not belong to dir %q"改为带 dirIdx(名字已不可得),可读性略降,可接受;调试时可用 `-vv` flist 日志对照。

### 2. touch-up 集合:`retouch []*File`

生成器建目录分支(generator.go:186-191)检测到不可写目录时,顺手把 **f 本身** 收进 `inc.retouch`:

```go
if mode&syscall.S_IWUSR == 0 {
    rt.retouchDirPerms = true
    mode |= syscall.S_IWUSR
    inc.retouch = append(inc.retouch, f)   // 新增
}
```

`Do` 里的收尾改为:

```go
if rt.retouchDirPerms {
    if err := rt.touchUpDirs(rt.inc.retouch); err != nil { ... }
}
```

`touchUpDirs` 里的 `S_IWUSR > 0` 跳过分支变成防御性冗余(集合构造条件与它逐字相同),保留不动,双保险。

与现状行为逐一对照,确认集合恒等:

| 场景 | 现状 touchUpDirs 处理 | 新方案 retouch 集合 |
|---|---|---|
| 可写目录 | 跳过(S_IWUSR>0) | 不收集 |
| 不可写目录 | setPerms | 收集(生成器同一条件) |
| DryRun | 全部跳过 | 目录分支在检测前已 return(generator.go:163-165),不收集;`retouchDirPerms` 不会被置位,Do 也不调用 |
| 非目录 | 跳过(S_IFMT 检查) | dirs/retouch 本来只含目录 |

注意收集点必须在 DryRun 提前 return **之后**、与 `retouchDirPerms = true` **同分支**——两者由同一个条件驱动,天然一致。

## 不变量与风险

1. **不依赖 dirIdx 单调性**:校验保留对 `len(dirHashes)` 的边界检查,与现状完全一致。发送端段顺序单调(DFS)是实现细节,不作为正确性依据。
2. **单写者纪律不变**:`dirHashes` 帧循环独占写(替代原 dirs 的角色);`retouch` 生成器独占写、`eg.Wait()` 后读。零新增锁。
3. **失败模式是响亮报错**:哈希不匹配/边界越界 → pushEntries 返回 error → 传输中止,与现状相同,不会静默错数据。
4. **retouch 挂在 inc 上还是 Transfer 上**:挂 `inc.retouch`(与 dirs 同生命周期),Transfer 只保留 `retouchDirPerms` 标志不变,改动面最小。
5. **测试断言迁移**:incremental_test.go 有 4 处 `inc.dirs` 断言(dirs 数量/首条目名),改为断言 `dirHashes` 长度与 retouch 集合。
6. **内存换 Debug 输出**:帧循环的 `%+v` 日志(incremental.go:449)打印的是 byNdx 里未释放的条目,不受影响。

## 预期收益

| 树规模 | 现状(dirs 全量) | 方案C+ 后 |
|---|---|---|
| 83k 目录(OpenWrt SDK) | ~13-16 MB | ~2 MB(24 B × 83k + retouch 少量) |
| 百万目录 | ~150-200 MB | ~24 MB |

`--delete` 场景额外收益:deleteList 不受影响(names 早已独立);dirs 的释放路径与 --delete 无交互。

## 实施清单

1. `incremental.go`:字段 `dirs []*File` → `dirHashes [][2]uint64` + `retouch []*File`;pushEntries 写哈希;注释更新(消费点分析结论写进字段注释)。
2. `generator.go`:建目录分支收集 retouch;touchUpDocs 遍历对象改 retouch(签名不变)。
3. `do.go`:收尾 `fileList = rt.inc.dirs` → `rt.inc.retouch`(retouchDirPerms 为 false 时不必赋值)。
4. `flist.go` 或新文件:dirHash128 实现 + 单测(已知向量 + 碰撞 sanity)。
5. 单元测试:更新 incremental_test.go 4 处断言;新增:不可写目录收集、可写不收集、dry-run 不收集、哈希校验拒绝错位段(构造 dirIdx 指错父目录的段)。
6. 互操作回归:深树矩阵(含 --delete、dry-run、重同步)+ 带 0700 目录的用例(**新增**:源树含不可写目录,验证 touch-up 后权限恢复——现矩阵可能未覆盖)。
7. 内存验证:receiver_memory_test.go 阈值可再收紧(8 MB → 视实测),并用真实树实测 RSS。

## 工作量评级

核心改动 ~60-80 行,主要成本在测试(尤其不可写目录互操作用例)。风险评级低于已完成的逐条目释放(无跨 goroutine 计数,无协议行为变化,失败模式响亮)。
