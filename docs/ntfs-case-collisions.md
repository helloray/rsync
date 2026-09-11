# NTFS 大小写不敏感导致孪生文件反复重传

状态:已定位根因,未修复。2026-09-10 现场测试(C rsync 3.5.0 Linux 客户端推送 BCM6858 源码树到 Windows Go daemon)中发现。

## 现象

部分文件每一轮同步都会重传,大部分文件只传一次。反复重传的文件示例:

- `kernel/.../netfilter/xt_CONNMARK.h`、`xt_DSCP.h`、`xt_MARK.h`、`xt_RATEEST.h`、`xt_TCPMSS.h`
- `kernel/.../netfilter_ipv4/ipt_ECN.h`、`ipt_TTL.h`
- `kernel/.../netfilter_ipv6/ip6t_HL.h`
- `net/netfilter/xt_DSCP.c`、`xt_dscp.c`、`xt_DSCP.o`、`xt_dscp.o`、`.xt_DSCP.o.cmd`、`.xt_dscp.o.cmd`
- `iptables-1.4.21/extensions/libxt_DSCP.c`、`libxt_dscp.c`、`libip6t_HL.c`、`libip6t_hl.c`、`libxt_MARK.o`、`libxt_TOS.man` …

## 根因

**每一个反复重传的文件,在同目录下都有一个只差大小写的"孪生文件"**(`xt_DSCP.h` ↔ `xt_dscp.h`、`ipt_TTL.h` ↔ `ipt_ttl.h`、`libxt_MARK.c` ↔ `libxt_mark.c`、`ip6t_HL.h` ↔ `ip6t_hl.h`……)。这类成对命名在 Linux 内核源码树里很常见(netfilter 内核/用户态头文件尤其多)。

而 NTFS 默认**大小写不敏感**:两个名字在 Windows 上是同一个文件。已验证:

1. 同步目标目录里只有小写版(`xt_connmark.h`/`xt_dscp.h`/`xt_mark.h`),大写版没有作为独立文件存在。
2. NTFS 行为演示:`echo A > xt_DSCP.h; echo B > xt_dscp.h` → 目录里只有一个文件,内容为 B(后写覆盖先写)。

### 为什么表现为"每轮重传"而不是"只错一次"

1. 同步按 f_name_cmp 排序写入:大写(`D` = 0x44)排在小写(`d` = 0x64)之前,先写 `xt_DSCP.h`,再写 `xt_dscp.h`;两者落在**同一块磁盘文件**,最终内容与 mtime 都是小写版的。
2. 下一轮同步,生成器逐条 `skipFile` 比较(大小 + mtime):
   - `xt_DSCP.h` vs 磁盘文件(小写版内容/时间)→ 不匹配 → 重传,并把磁盘 mtime 改成大写版的时间戳;
   - `xt_dscp.h` vs 磁盘文件(刚被覆盖成大写版的时间戳)→ 不匹配 → 也重传。
3. 如此每一对孪生文件**永远双双重传**;没有孪生的文件一次比较即稳定——正是"大部分文件不会"的原因。

### 隐性数据丢失

磁盘上只保留排序在后的小写版内容,**大写版内容被静默丢失**。

### 变体:文件↔目录碰撞(2026-09-11 现场发现,已修复语义)

比孪生文件更严重的变体:源树同时有**文件 `INSTALL`** 和**目录 `install/`**(openssl-1.0.2a,`src/netmgr/sfumgr/src/service/src/features/`)。Linux 上二者共存;NTFS 上撞成同一个名字后:

1. inc-recurse 下目录条目先处理:发现目标端同路径是文件 → "unlinking to make room for directory" 删文件建目录 → `install/` 的内容正常同步进来;
2. 轮到文件 `INSTALL` 时:发现目标端是**非空目录** → 旧代码 `Remove` 失败("The directory is not empty")并**中止整个传输**。

C rsync 在此场景(generator.c:2148 → delete_item)的行为:用户的 `rsync -rptgoDv` 不带 `--delete`,`del_opts` 无 `DEL_RECURSE`,`delete_dir_contents` 对非空目录返回 DR_NOT_EMPTY → 打印 "cannot delete non-empty directory" 并**跳过该文件、传输继续**(exit 23 收尾);空目录则被 rmdir 掉、文件正常落盘。

已按 C 语义修复(generator.go 的 make-room 分支):非空目录挡路 → 记日志、`IOErrors`+`skipCount` 计数、跳过该文件继续传;空目录/其他非普通条目照旧删除落盘。修复后该树进入稳定状态:`install/` 目录内容完整同步,文件 `INSTALL` 每轮跳过并计入错误(不再中止传输,也不再与目录互相破坏)。注意这与孪生文件的重传问题不同——文件↔目录碰撞是**稳定跳过**,不会无限重传。

### 变体之二:生成器/接收端竞态(2026-09-11 现场发现,已修复)

上面的修复只覆盖"文件条目处理时目录已存在"的顺序场景。inc-recurse 下文件条目按排序先于目录条目处理(`INSTALL` < `install`),生成器发出文件传输请求后**继续往下走**,把 `install/` 目录建了出来;而文件数据是接收端 goroutine 异步落盘的——等它提交时路径已被目录占据,`renameat ...: Access is denied`(Windows 上 rename 文件覆盖目录即此错),整个传输中止。现场日志证实:`receiver.go: opening local file failed, continuing: ...INSTALL is a directory` 之后一秒 `renameat ... Access is denied`(iptables-1.4.21)。

修复:接收端提交(rename)失败且目标已是目录时 → 与生成器同语义,跳过 + `IOErrors`/`skipCount` 计数,丢弃临时文件,传输继续(receiver.go 的 commit 错误分支)。另一处配套:生成器目录分支"腾地方"删除只读文件时,Windows 会 Access denied(POSIX unlink 忽略文件自身权限位)→ `removeMakeRoom` 先清只读位再删。

## 参照:C rsync 的行为

cygwin/msys2 版 C rsync daemon 能同时存下两个孪生文件:cygwin 3.1+ 在创建目录时自动设置 Windows 逐目录大小写敏感标志(FileCaseSensitiveInfo,Windows 10 1703+)。因此 C daemon 没有此问题;原生 Windows 程序(包括 Go daemon)默认存不下。

## 对策(候选,未实施)

| 方案 | 效果 | 代价/风险 |
|---|---|---|
| A. 接收端 MkdirAll 时对新建目录启用逐目录大小写敏感(`SetFileInformationByHandle` + `FileCaseSensitiveInfo`) | 真正同时保存两个文件,对齐 Linux 语义 | 需验证 Windows 10 LTSC 2019 (17763) + 进程权限;已有目录要补开属性;部分系统可能要求 WSL 可选组件 |
| B. 检测大小写冲突,跳过 + 计数(与 23ca054 冒号名的 per-file 错误语义一致) | 消除无限重传,诚实报告"存不下" | 树中缺少孪生文件(原生 Windows 本就存不下) |
| C. 不处理(现状) | — | 每对每轮重传 2 次 + 大写版内容静默丢失,最差 |

建议顺序:先做 B(小、稳),再实验 A 在目标机器上是否可行,可行则以 A 作为增强(B 保留为 A 不可用时的回退)。

实现 B 时的注意点:冲突检测需要按"目录 + 小写名"建索引对照同一 flist 内的兄弟条目,以及磁盘上已存在的大小写变体;跳过语义应复用 `rt.skipCount` / `MSG_ERROR_EXIT(RERR 23)` 的既有路径(见 `internal/receiver/generator.go` 的 unrepresentable 分支与 `internal/receiver/do.go` 的收尾逻辑)。

## 相关提交

- `23ca054` fix(receiver): skip unrepresentable names instead of aborting the transfer —— 冒号文件名的 per-file 跳过语义,本问题的方案 B 将复用该路径。
- `7d80f9f` fix(receiver): accept Windows reserved device names via SafeRoot fallback —— 保留设备名(AUX 等)的 `\\?\` 回退,与本问题同属"Linux 文件名在 Windows 上的表示"主题。
