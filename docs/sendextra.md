  你的判断是对的：预扫描把增量递归的 sender 端内存收益完全抵消了

  C 版本的做法（_c-rsync/flist.c，刚核对过）：

  - send_file_list 只发送顶层条目作为初始列表，文件树的其余部分根本不在内存里
  - 之后 sender 循环里 send_extra_file_list → send1extra →
    send_directory：每次只扫描一个目录，凑够 ~1000
    条窗口就停（flist.c:2396-2441）
  - 生成器消费完的 flist 被 flist_free 真正释放（内存池归还），不在 sender 常驻

  所以 C sender 的常驻内存 ≈ 目录树（dir_flist，只存目录条目不存文件）+ 1000 
  条窗口。10 万文件、1100 个目录的树，C sender 也就几 MB 的量级——而不是 Go 的 94
  MB。

  Go 版本：internal/sender/flist.go 先走完整棵树 → 全局 FNameCmp 排序 →
  newIncSched 按目录切段、预分配 dir
  索引空间。这一套要求先看到全树才能算出段链和 dirIndex，于是 sender 端每条目 ~1
  KB 的常驻开销，100 万文件 ≈ 1 GB。

  准确地说：预扫描“多余”，但不白做

  - 线上协议行为与 C 完全一致（段的切分、ndx 链、dir 索引都对齐了 C），interop
    测试过了——浪费的只是 sender 内存，不是协议兼容性
  - 它是 Phase E 的实现简化：全局排序后切段、索引预编号，比 C
    那套“边扫边编号、增量维护 dir 树”的增量逻辑好写得多，也少一类
    bug（今天的死锁就是增量记账出的错）
  - receiver 端（客户端拉取）收益是真的：42–49 MB/100k，且不用等全列表

  如果要修

  方向就是照 C 的 send1extra 模型改 Go sender：topUp
  时按需扫描下一个目录、动态追加段、dirIndex
  增量分配，已完成的段真正释放。改动集中在
  internal/sender/flist.go（去掉全树预走）和
  incremental.go（段生成从“预切”变“现切”），是这几周以来最实质的一次重构，但能把
  sender 内存从 O(总条目) 降到 O(目录数 + 窗口)。

  要我做这个重构吗？等分类器恢复后我也可以先把 C daemon 的实测数字补上（预期
  100k 文件下在几 MB 量级），作为重构前的基线。