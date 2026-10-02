# diff-merge-engine-go

一套可用于多人协作的**差异生成、补丁应用与三方合并**引擎（纯 Go，无第三方
依赖）。它解决“整体覆盖文件”带来的三个问题：

- 上下文行号位移后无法应用；
- 双方改到同一区域无法区分自动合并与真冲突；
- 出错 / 中断后回不到一个一致的已提交版本。

## 包结构

| 文件 | 职责 |
| --- | --- |
| `lines.go` | 流式按行读取、EOL/空白规范化、FNV-1a 行哈希、内存上界的 `Spool`（超预算溢写到临时文件） |
| `diff.go` | 线性空间 Myers（middle snake）最短编辑脚本，O(N+M) 整数内存 |
| `patch.go` | 统一差异补丁的生成、严格解析、反转、与文件名无关的稳定 `ID`，可移植线格式 |
| `apply.go` | 容忍偏移/空白/fuzz 的补丁应用、歧义与越界检测、反向回退、原子落盘 |
| `merge3.go` | 确定性三方合并与结构化冲突标记 |
| `store.go` | 内容寻址 blob + fsync 追加日志 + OS 文件锁的协作存储，幂等去重、崩溃恢复 |
| `errors.go` | 可区分的错误类型 |

## 快速使用

```go
import dm "github.com/huchangjun118/diff-merge-engine-go"

// 1) 生成补丁
old := dm.NewLineSource(strings.NewReader(oldText), dm.StreamOptions{})
cur := dm.NewLineSource(strings.NewReader(newText), dm.StreamOptions{})
patch, _ := dm.GeneratePatch(ctx, old, cur, dm.PatchOptions{
    OldName: "f", NewName: "f", Context: 3,
})
wire, _ := patch.Encode()           // []byte，可跨环境传递
p, _ := dm.ParsePatch(bytes.NewReader(wire))

// 2) 应用（容忍行号移动）
var out bytes.Buffer
_, err := dm.Apply(ctx, target, p, &out, dm.ApplyOptions{MaxOffset: 1000})

// 3) 回退
var back bytes.Buffer
_, err = dm.ApplyReverse(ctx, patched, p, &back, dm.ApplyOptions{}) // == target

// 4) 三方合并
var merged bytes.Buffer
_, err = dm.Merge3(ctx, base, ours, theirs, &merged, dm.MergeOptions{})
var ce *dm.ConflictError
if errors.As(err, &ce) {
    // merged 已写入带标记的完整结果；ce.Conflicts 是结构化冲突位置
}
```

## 补丁的线格式（可移植性）

- 头部固定为 `--- old` / `+++ new`，hunk 头为 `@@ -a,b +c,d @@`。
- 每条 body 行首字符为空格（上下文）、`-`（旧）、`+`（新）。
- **物理换行一律使用 LF 写出**；内容里的 CRLF 把 CR 原样保留在内容中，
  因此不会因平台差异丢失或多出 `\r`。
- 内容行没有终止符时，下一行输出 `\ No newline at end of file` 显式标记；
  “文件末尾是否有换行”由此精确保留，应用结果在任何环境都一致。
- 补丁 `ID()` 只对内容变化（hunk 体与计数）取 SHA-256，与文件名、生成位置
  无关；相同改动在不同环境产生相同 ID，可用于幂等去重。

## 应用匹配规则

对每个 hunk 按以下顺序定位，全程以 hunk 在补丁中声明的旧侧行号为基准：

1. **精确就位**：声明位置处上下文与删除行完全匹配（在 `Compare` 规则下）。
   只要声明位置逐字吻合就采用它，即使窗口内别处存在相同片段（确定性规则，
   不依赖“猜最近”）。
2. **窗口内全模式搜索**：当声明位置不匹配时，在
   `[declared-MaxOffset, declared+MaxOffset]` 范围内
   寻找完整 hunk 旧侧模式。
   - 恰好一个匹配：应用，记录实际偏移；
   - 两个及以上匹配：返回 `*AmbiguousMatchError`（含候选行号），**绝不猜测**；
   - 零个匹配且不是无限制模式：进入下一步。
3. **fuzz 匹配**：丢弃 hunk 两端各 `Fuzz` 行上下文后再在窗口内搜索；fuzz
   同时对称地收紧旧侧与新侧窗口，匹配起点按被丢弃的前导上下文数补偿。
4. 仍找不到：
   - 目标中根本不存在该内容 → `*BoundsError`；
   - 放宽到无限制后能匹配但超出 `MaxOffset` → `*OffsetExceededError`。

`LineCompareOptions` 控制内容等价：

- `NormalizeEOL`：CRLF/CR/LF 终止符等价；
- `IgnoreTrailingWhitespace`：忽略行尾空格与制表；
- `IgnoreAllWhitespace`：忽略所有水平空白（隐含上一项）。

新增（`+`）行永远逐字采用补丁内容，规范化只影响“匹配”，不会静默改写结果。

多个 hunk 按补丁顺序定位，后一个 hunk 的搜索起点不早于前一个匹配结束位置，
避免重叠错位。

**失败语义**：任何错误发生在第一个输出字节之前（先完整定位再写出），因此
`Apply` 不会产生半成品；`ApplyToFile` 写临时文件 → fsync → 同目录 rename，
取消、超时、失败都只删除临时文件，原文件不动。反向应用失败时同样保持原内容
完整，可安全重试。

## 三方合并与冲突语义

`Merge3(base, ours, theirs)`：

- 分别计算 base→ours 与 base→theirs 的最短编辑脚本，把替换（插入+删除）配对，
  在 base 轴上用两侧全部编辑切点切成互不重叠的对齐区间；
- 每个区间分类为：双方都未改 / 仅 ours 改 / 仅 theirs 改 / 双方都改。
  - 仅一方修改：自动采用该方；
  - 双方都改但结果逐行相同：视为同一次修改，自动合并，不产生冲突；
  - 双方都改且结果不同：真冲突。
- 纯插入在同一锚点：内容相同自动合并；内容不同产生冲突；在不同锚点的插入
  各自归位。
- 删除 vs 修改同一区域是冲突；双方删除同一行自动合并。

冲突输出（双方原始内容完整保留）：

```
<<<<<<< ours
<ours 在该区域的原始行>
=======
<theirs 在该区域的原始行>
>>>>>>> theirs
```

标签（`OurLabel`/`TheirLabel`，默认 `ours`/`theirs`）与 ours/theirs 顺序只由
选项决定，与谁先到达无关。冲突位置通过 `ConflictInfo`（base/ours/theirs 的
1-based 起止行，以及输出中标记块的起止行）结构化返回，并以
`*ConflictError` 与“补丁畸形/编码错误/越界”等输入类失败严格区分。

判定只依赖 `(base, ours, theirs)` 三个内容，因此**可复现且与合并顺序无关**：
非重叠改动无论以什么顺序提交，最终 blob 的内容哈希相同。

## 协作存储（Store）

`OpenStore(dir)` 打开目录并立即执行恢复：

```
dir/
  blobs/<ab>/<cdef...>   内容寻址 blob（SHA-256），临时文件+fsync+rename 落盘
  journal/NNNN-*.log     每提交一个仅追加、fsync 的 JSON 段（WAL）
  HEAD                  当前提交指针（原子替换 + 目录 fsync）
  tmp/                  临时目录，打开时清空残留
  lock                  OS 文件锁（flock，非阻塞）
```

- **串行化**：进程内互斥锁 + `flock` 跨进程互斥；并发提交被串行应用。
- **幂等去重**：按 `PatchID`（缺省取补丁内容 ID）去重，重复提交返回原提交
  （`Duplicate == true`），不产生新版本；乱序到达不会重复或错乱。
- **基于最新版本 rebase**：晚到补丁用 `(submitterBase, base+patch, HEAD)`
  做三方合并落到最新版本；非重叠改动结果与到达顺序无关。
- **原子性**：blob 与 journal 记录要么都 fsync 持久、要么都没有；HEAD 最后
  更新。崩溃后 `Recover`/重新 `OpenStore` 重放日志，截断的尾部记录（torn
  write）被忽略，blob 逐一校验 SHA-256，恢复到最后一个完整提交。
- `KeepConflictsInHistory` 为真（默认）时冲突也作为带标记版本提交并记录
  `Conflict=true`；为假时冲突提交被拒绝且不写入任何内容。

## 大文件与资源上界

- 行读取支持 `StreamOptions{MaxLineBytes, MaxLines}`，超限返回 `*LimitError`，
  读缓冲固定大小，不依赖无限增长的 Scanner 缓冲。
- apply / merge 把行内容放进 `Spool`：内存保留量由 `MaxMemoryBytes`（默认
  4 MiB）封顶，超出的行溢写到临时文件，任意时刻只额外保留一行；行索引为
  定长记录。处理结束（含取消）临时目录被删除。
- diff 使用线性空间 Myers，编辑过程不持有全部内容的额外副本。

## 错误分类（互不混淆）

| 错误 | 含义 |
| --- | --- |
| `*MalformedPatchError` | 补丁结构错误（头缺失、hunk 计数不符、前缀非法、未规范化顺序等） |
| `*EncodingError` | `RequireUTF8` 下输入非法 UTF-8 |
| `*AmbiguousMatchError` | 窗口内多个等价位点，无法唯一确定 |
| `*OffsetExceededError` | 偏移超出 `MaxOffset` |
| `*BoundsError` | hunk 引用内容在目标中不存在 |
| `*LimitError` | 超过行长度/行数等资源限制 |
| `*ConflictError` | 三方合并的真冲突（不是失败） |
| `*ChecksumError` / `*NotFoundError` / `*StateError` | 存储校验、对象缺失、非法状态 |

所有写入类操作在这些错误上都**不做部分写入**。

## 已知限制

- 行级（非字符级）差异与合并；单行为一个最小单位。
- 当前文件锁使用 POSIX `flock`（Linux/macOS 等）。
- 冲突标记是标准文本标记，不额外输出三方合并的 base 段；base 范围通过
  `ConflictInfo` 提供。
