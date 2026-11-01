# 内置向量索引

覆盖：两个子系统共用的 `vector` 表、行编码、加载与重建、写入协议、top-k 查询、模型与维度变更、容量、故障修复与测试注入缝。不覆盖：记忆抽取（见 [`memory.md`](memory.md) 第 9 章）、知识库抓取与分块（见 [`rag.md`](rag.md) 第 10 章）、Embedding HTTP 协议（见 [`llm.md §12.2`](llm.md#122-embeddings)）。

## 11. 内置向量索引

### 11.1 目标与约束

- 索引完全在插件进程内，不依赖外部向量数据库。
- 行持久化在 `bot.TableStorage` 的单一逻辑表 `vector`；索引是启动时从该表重建的内存结构。
- 表能力来自 `PluginContext.Tables`，与 `Storage` 共用 `storage` 权限。`memory_enabled || rag_enabled` 时 `Tables` 必须非 nil，否则 `Setup` 返回 `chat-agent: 需要 TableStorage 支持`。
- 逻辑表名 `vector` 加 `plugin:chat-agent:` 前缀后不超过 191 字节；行键不带前缀，`m:`/`d:`/`c:` 后接 32 位小写 hex，`meta` 是固定行键。
- 表单次 `ListRows` 返回全部行；无分页、列 schema、TTL、事务。
- 单机内存索引，可见规模上限 `vector_max_records`；超过上限按淘汰或拒绝处理，不静默扩容。
- 支持两个 collection：`memory` 与 `knowledge`，共用同一套行写入协议和查询算法。
- 索引只存文本向量。图片记忆以视觉 LLM 识别文本的普通 `m:` 行参与检索；不存图片二进制、原始 URL 或图片向量。
- 启动异步加载；加载完成前聊天可以使用，但 RAG 跳过并记 `rag_status=loading`。

### 11.2 数据结构

内存记录：

| 字段 | 类型 | 含义 |
|---|---|---|
| `ID` | string | 32 位小写 hex |
| `Collection` | string | `memory` 或 `knowledge` |
| `Text` | string | 原始文本，用于重建与 reindex；图片记忆为识别描述 |
| `Vector` | `[]float32` | 已 L2 归一化 |
| `Dims` | int | 维度 |
| `Model` | string | 生成向量的模型 |
| `Scope` | string | 记忆为 `channel`/`user`；知识块为空 |
| `ScopeKey` | string | 记忆的 scope key；知识块为空 |
| `DocID` | string | 知识块的文档 ID；记忆为空 |
| `ChunkIndex` | int | 知识块序号；记忆为 -1 |
| `Stale` | bool | 模型或维度与当前期望不匹配 |

`VectorRecord` 不单独落盘；它由 `vector` 表的 `m:<id32>` 与 `c:<id32>` 行构造。行形状见 [`architecture.md §4.4`](architecture.md#44-storage-模式)。

行键与来源：

| 行键 | 来源 | 说明 |
|---|---|---|
| `m:<id32>` | 记忆记录 | `Collection=memory`，`Scope`/`ScopeKey` 来自行内字段 |
| `d:<id32>` | 文档元数据 | 不进入索引，只提供 `chunk_ids` 引用与列表展示 |
| `c:<id32>` | 知识分块 | `Collection=knowledge`，仅在 `d:` 行引用时进入索引 |
| `meta` | 索引元数据 | 期望模型与维度，不是记录 |

`meta` 形状逐字固定，与 [`architecture.md §4.4`](architecture.md#44-storage-模式) 一致：

```json
{"version":1,"model":"...","dims":1536}
```

- `version` 固定为 1。
- `model`/`dims` 是索引期望的 Embedding 模型与维度。
- 没有 ID 数组；`meta` 不参与容量计数，也不决定哪些行可见。

编码：

- 向量为 little-endian `float32` 序列的 `base64.StdEncoding`，不带换行。
- 写入索引前做 L2 归一化；零范数向量拒绝并报错。
- `dims` 必须等于向量长度；不一致的行跳过并 warn。

### 11.3 加载

`Start` 阶段：

1. 用 `storage_timeout=2s` 调 `CreateTable(ctx, "vector")`；`bot.ErrTableExists` 视为成功，其它错误使 `Start` 返回 `chat-agent: 存储表初始化失败: <err>`。
2. 用 `table_load_timeout=30s` 调 `ListRows(ctx, "vector")`；错误使 `Start` 返回 `chat-agent: 存储表加载失败: <err>`。
3. 启动写协程与 Embedding 工作协程，置 `vector.state=loading`。
4. 用 `vector_load_workers=4` 并发解析行，顺序为 `d:` → `m:` → `c:` → `meta`。
5. 回收未引用分块、构造内存索引，置 `state=ready`。

解析规则：

- 单行 JSON、base64 或维度损坏时跳过并 warn，不阻断其它行。
- 行键前缀未知时跳过并 warn，不删除该行。
- 只有全部 `d:` 行解析成功时才回收未被任何文档 `chunk_ids` 引用的 `c:` 行；任一 `d:` 行非法时本轮跳过回收、保留分块并 warn。
- 文档 `chunk_ids` 引用的分块不存在时 warn，文档与其余分块继续加载。
- `meta` 缺失、非法或 `version != 1` 时按缺失处理：`model` 取 `embedding_model`、`dims` 取 `embedding_dimensions`，再结合已有行推断，尽力写回；写回失败只 warn。
- 行级错误不使 `Start` 失败；只有 `ListRows` 本身失败才失败。

等待语义：

- RAG 检索前最多等 `vector_ready_timeout=2s`；仍未就绪则跳过检索，`rag_status=loading`。
- `vector` 状态命令不等待，直接报告当前状态。

状态：

```text
vector: state=<ready|loading|disabled> model=<model> dims=<n> memory=<n> knowledge=<n> stale=<n>
```

- `disabled`：`memory_enabled=false && rag_enabled=false`，不建表、不加载。
- `loading`：启动加载尚未完成，计数是已解析的部分。
- `ready`：加载完成，包括空表。
- `model`/`dims` 报告 `meta` 的期望值；`meta` 缺失或非法时报告配置值，维度为 0。
- `memory`/`knowledge` 只计非 stale、可检索的行；`stale` 计全部 stale 的记忆行与分块行。

加载期间命令行为：

- `memory` 全部子命令、`knowledge` 全部子命令、`jobs run consolidate`、`vector reindex` 输出 `chat-agent: 向量索引未就绪`。
- `vector status`、`jobs status`、`status`、`help`、`policy`、`list`、`admins`、`on/off/reset` 不受影响。
- `jobs run cleanup` 不依赖索引，允许执行。
- 功能未启用文案优先于未就绪文案。

### 11.4 写入/删除协议

所有写入经单写协程串行执行；`CreateTable` 与 `ListRows` 只在 `Start` 加载阶段直接调用。

- 行写入统一 `putRow`：先 `UpdateRow`；返回 `bot.ErrNotFound` 时改 `InsertRow`；返回 `bot.ErrRowExists` 时重试一次 `UpdateRow`；其它错误返回 `storage_error`。
- 删除用 `DeleteRow`；行不存在按成功处理。
- 不做全表重写，不做跨行事务。
- 内存索引只在行写成功后切换快照；失败时保持旧快照。
- 确定性 ID 使重试幂等；重复写同一 ID 覆盖同值。
- 崩溃最多留下未被引用的 `c:` 行，不会出现指向缺失块的可见文档；回收见 §11.8。
- 写入失败返回 `storage_error`；调用方据此决定是否推进记忆水位或知识库结果。

知识库提交点是 `d:<id32>` 行：

- 新增：先写全部新 `c:` 行，再写 `d:` 行（提交）；提交前失败只留下未引用块。
- 更新：先写新 `c:` 行，再写新 `d:` 行（提交），最后 `DeleteRow` 旧块；删旧块失败只 warn。
- 删除：先 `DeleteRow` `d:` 行（提交移除），再删旧块；删块失败只 warn。
- 顺序与失败语义见 [`rag.md §10.5`](rag.md#105-索引与更新) 与 [`architecture.md §4.7`](architecture.md#47-表一致性与孤儿回收)。

写队列与 `Stop` 的顺序见 [`architecture.md §3.4`](architecture.md#34-关闭顺序) 与 §4.6。写入失败记 warn 与计数，不 panic。

### 11.5 查询算法

1. 校验查询向量非零，做 L2 归一化。
2. 按 collection 与过滤条件筛选候选：
   - `memory`：`scope=channel` 且 `scope_key=当前频道`，或 `scope=user` 且 `scope_key` 在本批发送者集合中。
   - `knowledge`：全部可检索知识块。
   - `Stale=true` 的行一律排除。
3. 跳过 `dims` 与查询向量维度不同的行并 warn；不参与点积，不进入候选。
4. 对候选逐个计算点积；归一化后点积等于余弦相似度。
5. 用大小为 k 的最小堆维护 top-k。
6. 按分数降序、分数相同按 ID 升序返回。
7. 调用方再按 `memory_min_score` 或 `knowledge_min_score` 过滤。

复杂度为 `O(N·D)`。`N=10000`、`D=1536` 时单次查询约 1536 万次乘加；这是可接受的暴力检索规模。索引不做近似、不建树、不分片。

### 11.6 模型与维度变更

判定 stale：

- `record.Model != embedding_model`。
- `embedding_dimensions > 0` 且 `record.Dims != embedding_dimensions`。
- `embedding_dimensions == 0` 且 `meta.dims > 0` 时 `record.Dims != meta.dims`。

处理：

- stale 行保留在表中且计入容量，但不参与检索。
- `/chat vector reindex` 只处理 stale 行：用行内 `text` 重新嵌入，更新同 ID 行的 `vector`、`dims`、`model`。图片记忆也只重嵌入识别文本，不重新下载图片或请求图片 Embedding。
- 全部 stale 处理成功后写 `meta`（`version=1`、当前 `embedding_model` 与维度）；部分失败时保留旧 `meta`，失败行保持 stale。
- 没有 stale 时输出 `vector: 完成 reindexed=0`，不写 `meta`。
- reindex 期间新写入使用当前配置模型；本次快照不覆盖新写入。
- reindex 是异步任务，立即返回 `vector: 已开始重建`；完成输出 `vector: 完成 reindexed=<n>`。
- 部分行失败时仍发完成消息，`reindexed` 只计成功数；失败行记日志，下一次可重试。
- 加载期间或 `state=disabled` 时触发输出 `chat-agent: 向量索引未就绪`。
- 已有 reindex 运行时再次触发输出 `chat-agent: 忙碌: vector`。

### 11.7 容量与淘汰

计数：

```text
visible = 可见 m: 行数 + 被合法 d: 行引用的 c: 行数
visible <= vector_max_records
```

- 计入 stale 记忆行与被引用的 stale 分块行。
- `d:`、`meta` 与未被引用的 `c:` 行不计数。
- 更新期间表物理行数可临时超过上限（先写新块、后删旧块），回收后回落。

插入记忆：

1. 若 `visible + 新记录数 <= vector_max_records`，直接写入。
2. 否则优先淘汰 stale 记忆，再按 effective 从低到高淘汰当前记忆。
3. effective 使用 [`memory.md §9.4`](memory.md#94-定时整理) 的衰减公式；相同 effective 按 ID 升序。
4. 淘汰走 `DeleteRow`；删除失败返回 `storage_error`，本次不插入、不推进水位。
5. 若没有可淘汰的记忆（名额全部被知识块占用，包括 stale 知识块），本次写入失败 `vector_full`。

插入知识块：

- `projected = visible - len(old_doc_chunks) + new_chunk_count`；超过上限时不写新块，返回 `vector_full`。
- 不淘汰任何知识块或记忆。
- 单文档分块写入前先估算；不足时整批失败，不写半成品。

reindex 不增加记录数；删除文档释放其全部分块名额。

### 11.8 故障与修复

| 故障 | 行为 | 修复 |
|---|---|---|
| `Tables` 为 nil 且功能开启 | `Setup` 返回 `chat-agent: 需要 TableStorage 支持` | 使用内置后端或自实现 TableStorage |
| `CreateTable` 失败 | `Start` 返回 `chat-agent: 存储表初始化失败: <err>` | 修复存储后端后重启 |
| `ListRows` 失败或超时 | `Start` 返回 `chat-agent: 存储表加载失败: <err>` | 修复存储后端后重启 |
| 单行 JSON/base64/维度损坏 | 跳过并 warn，索引继续 | 重新写入同 ID 行 |
| 行键前缀未知 | 跳过并 warn，不删除 | 手动清理未知行 |
| 任一 `d:` 行非法 | 跳过回收并 warn，保留未引用分块 | 修复或删除该文档行后重启 |
| 未引用 `c:` 行 | 不加载、不可见、不计数 | 启动回收；失败则下次启动重试 |
| `meta` 缺失或非法 | 按配置与已有行推导，尽力重写 | 自动；失败只 warn |
| 模型/维度变更 | 标记 stale，排除检索 | `/chat vector reindex` |
| 容量满 | 记忆可淘汰；知识块拒绝 | 删除文档或提高上限后重启 |

`/chat memory reset` 删除全部 `m:` 行；`/chat knowledge del` 删除对应 `d:` 行与其 `chunk_ids` 的 `c:` 行；`/chat vector reindex` 只处理表中仍存在的 stale 行。未引用 `c:` 行由启动回收处理。

### 11.9 测试注入缝

以下依赖必须可替换，测试用手写桩：

| 注入点 | 用途 |
|---|---|
| `now func() time.Time` | 衰减、TTL、时间戳 |
| `embedder` | 固定维度向量、错误、超时 |
| `Storage` | 内存 fake、故障注入 |
| `Tables` | 表 fake：`CreateTable`/`ListRows` 错误、`bot.ErrRowExists`、`bot.ErrTableNotFound` |
| `vectorLoadWorkers` | 默认 4，测试可设为 1 |
| `vectorReadyTimeout` | 默认 2s，测试可缩短 |
| `tableLoadTimeout` | 默认 30s，测试可缩短 |
| `randFloat` | 随机参与与概率边界 |

不引入第三方 mock 库；并发与关闭用 `sync.WaitGroup` 与显式计数验证。

## 相关文档

- 表与行形状：[`architecture.md §4.4`](architecture.md#44-storage-模式)。
- 提交与回收：[`architecture.md §4.7`](architecture.md#47-表一致性与孤儿回收)。
- 记忆容量与衰减：[`memory.md §9.3-9.4`](memory.md#93-嵌入与合并)。
- 知识库写入顺序：[`rag.md §10.5`](rag.md#105-索引与更新)。
- Embedding 协议：[`llm.md §12.2`](llm.md#122-embeddings)。
- 配置键：[`configuration.md §13.1`](configuration.md#131-权威配置键表)。
- 测试矩阵：[`testing.md §15.2-15.3`](testing.md#152-单元测试矩阵)。
