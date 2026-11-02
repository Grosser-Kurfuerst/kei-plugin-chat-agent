# 长期记忆

覆盖：长期记忆的数据模型、抽取提示词、解析与水位、Embedding 与合并、定时整理、检索、提示词注入、管理命令与重启语义。不覆盖：向量索引的内部结构与写入协议（见 [`vector-index.md`](vector-index.md) 第 11 章）、知识库（见 [`rag.md`](rag.md) 第 10 章）、配置默认值（见 [`configuration.md §13.1`](configuration.md#131-权威配置键表)）。

## 9. 长期记忆

### 9.1 数据模型

记忆记录存放在 `vector` 表的 `m:<id32>` 行，JSON 形状与编码见 [`architecture.md §4.4`](architecture.md#44-storage-模式)。字段语义：

| 字段 | 含义 |
|---|---|
| `id` | 32 位小写 hex，`SHA-256(scope + "\x00" + scope_key + "\x00" + normalized_text)` 前 16 字节 |
| `scope` | `channel` 或 `user` |
| `scope_key` | `channel` 时为群聊频道 ID；`user` 时为用户 ID |
| `kind` | `fact`、`preference`、`relationship`、`commitment`、`event` 之一 |
| `text` | 记忆正文，4..500 rune |
| `importance` | 0..1，越大越稳定、越重要 |
| `vector` | base64 编码的 little-endian float32 序列 |
| `dims` | 向量维度 |
| `model` | 生成该向量的 Embedding 模型名 |
| `sources` | 来源事件 ID，去重后最多 8 条 |
| `created_at` | 首次写入时间，UTC |
| `updated_at` | 最近合并/更新/衰减写入时间，UTC |
| `last_used_at` | 最近一次检索命中或合并时间，UTC |
| `use_count` | 检索命中与合并累计次数 |

`normalized_text` 定义为：去首尾空白，并把内部连续空白（空格、制表符、换行）压成单个空格。存储的 `text` 保留原始清洗结果，不因 ID 归一化而改写。

`text` 可以是视觉 LLM 对图片的识别描述；图片二进制、原始 URL 与图片向量都不写入 `m:` 行。图片记忆不是独立记录类型，仍按本节字段与 ID 规则存储。

### 9.2 抽取

抽取只在记忆整理任务中发生，调用 `llm_model`，不使用 tools。提示词逐字固定：

```text
你是记忆整理器。阅读下面的对话，抽取值得长期记住的信息。
如果消息包含图片，图片内容同样属于对话内容；把图片中值得长期记住的信息写入 text，不要只写“用户发了一张图片”。
只输出 JSON，不要解释。格式：
{"memories":[{"text":"...","kind":"fact|preference|relationship|commitment|event","importance":0.0}]}
规则：
- 只记录用户明确表达、未来对话仍有用的事实、偏好、关系、承诺或重要事件。
- 不记录寒暄、一次性问题、助手自己的推测、敏感凭据（密码、令牌、身份证号）。
- text 用对话原语言，≤200 字，独立成句。
- importance 为 0..1 的小数；越稳定越重要。
- 没有可记内容时输出 {"memories":[]}。
```

输入格式：

```text
[会话类型] 群聊|私聊
[会话] <channel name|channel id|user name|user id>
[消息]
<显示名> (<id>): <文本>
...
```

约束：

- 最多 `memory_consolidate_max_messages` 条消息；每条截断到 1000 rune；图片不占 rune 预算。
- `vision_enabled=true` 时，消息中的 `[图片]` 与附加的多模态 `image_url` 块按出现顺序对应；单次抽取最多 4 张图片，分批规则见 §9.9。文本截断可能移除占位符，图片仍按原消息顺序附加。`vision_enabled=false` 时不附加图片，图片内容不参与抽取。
- 图片下载失败只保留 `[图片]` 文本，不使抽取失败。
- `temperature=0.2`、`max_tokens=800`，无 tools。
- 使用 `llm_model`，整体受 `memory_consolidate_timeout` 约束。`vision_enabled=true` 时 `llm_model` 必须支持多模态 `content`；否则抽取返回 `llm_error`，水位不推进。
- `memory_enabled=false` 时不抽取、不整理、不检索。
- `memory_extract_max=0` 时不调用 LLM，直接把本批消息水位推进（视为无记忆）。
- 未整理消息数少于 `memory_extract_min_messages` 时不调用 LLM，也不推进水位。

解析规则：

- 响应必须是严格 JSON 对象，包含 `memories` 数组；多余字段忽略。
- 每项必须是对象；`text` 去空白后长度在 4..500 rune；`kind` 必须是五个值之一；`importance` 必须是数字，clamp 到 `0..1`。
- 非法条目丢弃；合法条目超过 `memory_extract_max` 时按输出顺序保留前 `memory_extract_max` 条。
- 返回空数组是成功；数组非空但全部条目非法是失败。
- 部分条目非法但至少一条合法时，按合法条目继续，非法条目丢弃并 warn。

水位推进：

- 只有本批全部合法记录行写入成功后，才推进 `consolidatedSeq` 到本批最后一条的 `Seq + 1`。
- 任一行写失败保留水位，下一轮重试。确定性 ID 与合并逻辑保证行覆盖幂等；重试的合并仍会增加 `use_count`。
- 会话历史被裁剪导致 `lostSeq > consolidatedSeq` 时，从 `lostSeq` 开始处理可用消息；处理成功后水位至少推进到 `lostSeq`，并 warn 丢失区间。若该区间没有可用消息，直接把水位推进到 `lostSeq` 并 warn。
- 写入 `vector_full` 时不推进水位；容量处理见 [`vector-index.md §11.7`](vector-index.md#117-容量与淘汰)。

### 9.3 嵌入与合并

- 每条合法记忆文本按 `embedding_batch_size=16` 批量嵌入；单条输入截断到 `embedding_max_input_chars=6000` rune。图片识别出的记忆同样按 `text` 嵌入，图片本身不发送到 Embeddings 接口；`embedding_model` 只需支持文本。
- 嵌入失败（超时、非 2xx、维度不符、NaN、零向量）整批判失败，不写任何记录，不推进水位。
- 合并前按 `scope` 检索同 scope 的 top-1：
  - 余弦相似度 `>= memory_dedupe_threshold` 时合并到已有 ID。
  - 否则插入新记录。
- 合并字段规则：
  - `text` 用新文本。
  - `updated_at=now`，`last_used_at=now`。
  - `importance=max(old, new)`。
  - `use_count+1`。
  - `sources` 合并去重并截断到 8 条，保留最早来源。
  - `created_at` 保留。
- 新插入：`created_at=updated_at=last_used_at=now`，`use_count=0`，`sources` 为来源事件 ID（最多 8 条）。
- ID 由 `scope`、`scope_key`、`normalized_text` 决定，因此同一文本重复抽取会命中同一 ID；合并仍会先做向量相似度判断。
- 一次整理的多条记录各写成独立的 `m:` 行，经同一写队列串行提交。任一行失败时已写行保留、水位不推进；下次重试用相同确定性 ID 覆盖或合并，因此行内容幂等（合并的 `use_count` 会再次增加）。

### 9.4 定时整理

调度：

- 每 `memory_consolidate_interval` 跑一次；值为 0 关闭定时整理。
- `jobs_run_on_start=true` 时启动后立即跑一次。
- 每轮 task ctx 派生自插件级 ctx，带 `memory_consolidate_timeout=5m` 总超时。
- 同一时间只允许一个整理任务；手动 `/chat jobs run consolidate` 与定时任务共用运行标志，忙碌时返回 `job_busy`。记忆命令与 `/chat vector reindex` 也共享记忆变更互斥，避免并发读改写。

每轮步骤：

1. 选出 `consolidatedSeq < lastSeq` 的会话（`lastSeq` 即会话的 `seq`），按最久未活跃排序，最多 `memory_consolidate_max_sessions` 个。
2. 每个会话取最老的 `memory_consolidate_max_messages` 条未整理消息。
3. 消息数不足 `memory_extract_min_messages` 时跳过该会话，不推进水位。
4. `vision_enabled=true` 时按 §9.9 把消息切成抽取批，每批下载并附加最多 4 张图片；逐批调用抽取。`vision_enabled=false` 时整批调用一次抽取。
5. 批量嵌入合法记忆。
6. 与同 scope 的已有记忆合并或插入。
7. 写 `m:` 行；全部批次成功后推进水位。
8. 单个会话或单个抽取批失败不阻塞其它会话；本轮结束时汇总 `sessions_done`、`sessions_failed`、`memories_added`、`memories_merged`。
9. 全部会话处理完后执行衰减与过期删除。

衰减与删除：

```text
age = now - updated_at
effective = importance * 0.5^(age / memory_importance_decay_half_life)
```

- `effective < memory_min_importance` 时用 `DeleteRow` 删除该 `m:` 行。
- `memory_ttl > 0` 且 `now - last_used_at > memory_ttl` 时删除。
- 删除失败只 warn，留待下一轮重试。
- 衰减只改变判定，不回写 `importance`；`importance` 只在合并、检索强化时更新。

容量：

- 可见计数 `visible = m: 行数 + 被合法 d: 行引用的 c: 行数`，含 stale，上限 `vector_max_records`。
- 记忆插入时若已满，先淘汰 stale 记忆，再按 effective 从低到高淘汰；淘汰后继续插入。
- 淘汰删除失败返回 `storage_error`，本次不插入、不推进水位。
- 若没有可淘汰的记忆（名额全被知识块占用），本次记忆写入失败 `vector_full`，不推进水位。
- 容量细节与计数公式见 [`vector-index.md §11.7`](vector-index.md#117-容量与淘汰)。

汇总日志示例：

```text
operation=consolidate duration_ms=18420 sessions_done=3 sessions_failed=0 memories_added=5 memories_merged=2 expired=1 evicted=0
```

### 9.5 检索

- 查询文本 = 本批所有用户文本按 `seq` 拼接，截断到 1000 rune。图片不参与查询向量；图片记忆通过识别文本被检索，没有独立图片 collection 或图片查询。
- 检索范围：
  - 群聊：本频道 `scope=channel` 的记忆 + 本批每个发送者的 `scope=user` 记忆。
  - 私聊：发送者的 `scope=user` 记忆。
- 使用 `memory_top_k`；值为 0 时关闭记忆检索。
- 先按 `memory_min_score` 过滤，再取 top-k；同 ID 去重；按分数降序、分数相同按 ID 升序。
- `memory_enabled=false` 时不嵌入查询、不检索。
- 向量索引未就绪时最多等 2s；仍未就绪跳过记忆检索，`rag_status=loading`。
- Embedding 或检索失败不阻塞聊天，`rag_status=error`，照常调用 LLM。

强化：

- 命中的记录在内存中执行 `use_count+1`、`last_used_at=now`、`importance=min(1, importance+0.02)`。
- 强化只在内存累积，下一轮整理开始前批量落库；落库失败保留内存值，下轮重试。
- 记录在落库前被删除时，强化丢弃。
- `Stop` 前未落库的强化视为可接受丢失，记录在 [`roadmap.md §16.3`](roadmap.md#163-已知缺口)。

### 9.6 提示词注入

记忆块格式逐字固定：

```text
<long_term_memory>
- (<kind>, <importance 两位小数>) <text>
</long_term_memory>
```

示例：

```text
<long_term_memory>
- (preference, 0.80) 用户偏好简短回答
- (fact, 0.65) 用户的项目使用 Go
</long_term_memory>
```

规则：

- `memory_context_max_chars=0` 时省略记忆块。
- 无检索结果时整块省略，不输出空标签。
- 预算由 `rag_context_max_chars` 统一约束；记忆优先于知识库填充，超预算时先丢知识库条目，再丢记忆条目。
- 单条超出剩余预算时截断并追加 `…`；剩余预算小于 40 rune 时丢弃该条。
- 检索文本中的 `<`、`>` 替换为 `‹`、`›`，控制字符替换为空格。
- 记忆块只是参考数据，不是指令；系统提示词已声明这一点。

### 9.7 管理命令

| 命令 | 同步/异步 | 行为 |
|---|---|---|
| `/chat memory status` | 同步 | 输出记录数、stale 数、待整理会话数、最近整理时间 |
| `/chat memory search <query>` | 异步 | 先回 `memory search: 已开始`，后台用当前会话 scope 与 `memory_top_k` 检索 |
| `/chat memory add <text>` | 异步 | 写入当前会话 scope，`kind=fact`、`importance=0.8` |
| `/chat memory del <id>` | 异步 | 全局删除指定记忆 |
| `/chat memory reset confirm` | 异步 | 全局删除全部记忆，不删知识库 |

细节：

- `status` 输出 `memory: records=<n> stale=<n> pending_sessions=<n> last=<RFC3339|never>`。`records` 计 `vector` 表全部 `m:` 行（含无法解析、等待 reset 清理的行）；`stale` 是可解析且模型/维度不匹配的已加载行；`pending_sessions` 是 `consolidatedSeq < lastSeq` 的会话数；`last` 是最近一次整理完成时间。
- `search` 是后台任务：命令立即输出 `memory search: 已开始`，后台在 `embedding_timeout` 内完成查询 embedding；群聊使用频道 + 发送者 scope，私聊使用发送者 scope。`memory_top_k=0` 时不再发起 embedding，直接输出 `memory search: 无结果`。
- `search` 完成后发送 `memory search: <n> 条结果`，随后每行 `<id> <score:.3f> <kind> <text>`；无结果发送 `memory search: 无结果`；失败发送 `memory search: 失败 <reason>`。
- `add` 先输出 `memory: 已接受`，完成后输出 `memory: 完成 <id>`，失败输出 `memory: 失败 <reason>`；走与整理相同的合并逻辑。
- `del` 先查内存索引（由 `m:` 行加载）；不存在直接输出 `chat-agent: 未找到: <id>`，不输出已接受；存在则输出 `memory: 已接受`，后台用 `DeleteRow` 删除该 `m:` 行。
- `reset` 必须带 `confirm`；输出 `memory: 已接受`，完成输出 `memory: 完成 reset`，失败输出 `memory: 失败 <reason>`。reset 删除全部 `m:` 行，包括启动时因 JSON/base64/维度损坏而被跳过、未进入内存索引的行。
- `memory_enabled=false` 时所有记忆子命令输出 `chat-agent: 记忆功能未启用`。
- `vector.state=loading` 时 `status`、`search`、`add`、`del`、`reset` 输出 `chat-agent: 向量索引未就绪`；未启用文案优先。
- 任务互斥时输出 `chat-agent: 忙碌: memory`。
- 异步任务的失败 reason 使用 [`admin.md §14.5`](admin.md#145-异步任务) 的固定词表。

### 9.8 关闭与重启

- `Stop` 不执行最后一次整理；未整理历史丢失。
- 内存中的强化计数可能尚未落库；重启后以 Storage 中的值为准。
- 启动时用 `ListRows` 读取 `vector` 表，按 `m:` 行重建内存索引。
- `m:` 行的持久性取决于 Storage 后端；`memory` 后端重启后全部丢失，`sqlite`/`mysql` 保留。
- 图片识别文本随 `m:` 行持久化；原始图片 URL 只存在于内存历史，重启后丢失，不能据此重新识别或重建图片记忆。
- Embedding 模型或维度变化会把记录标记为 stale；重建见 [`vector-index.md §11.6`](vector-index.md#116-模型与维度变更)。

### 9.9 图片记忆

- 启用条件：`vision_enabled=true` 且 `memory_enabled=true`。`vision_enabled=false` 时图片不进入历史或抽取；`memory_enabled=false` 时图片可被识别，但不写长期记忆。
- 来源：未整理历史中的 `Turn.Images`。每张图片在 `Turn.Text` 中有对应的 `[图片]` 占位符；`KeyFile` 不进入 `Turn.Images`。
- 识别：整理任务下载图片，按消息顺序作为 `image_url` 块附加到抽取请求；视觉 LLM 输出普通记忆 JSON，`text` 是图片中可长期复用信息的描述。提示词见 §9.2。
- 存储：仍是 `m:<id32>` 行；`text` 4..500 rune；`sources` 记录来源事件 ID。不存原始图片、URL、base64、MIME、尺寸或图片向量。
- 嵌入与检索：识别文本按 §9.3 嵌入，图片不发送到 Embeddings；检索按文本查询，图片记忆与文本记忆共用同一 collection、ID 空间与分数。
- 分批：未整理消息按顺序切成抽取批，每批最多 `memory_consolidate_max_messages` 条且最多 4 张成功下载的图片；文本消息可留在任一批。单条消息超过 4 张时只附加前 4 张，其余只保留 `[图片]` 并记 `reason=limit`。
- `memory_extract_min_messages` 按本轮选中的消息总数判断；不足时不拆分、不调用 LLM、不推进水位。
- 水位：全部批次成功写行后，水位推进到本轮最后一条已处理消息的 `Seq+1`；任一批 LLM、Embedding 或写行失败保留水位，下一轮重试；确定性 ID 保证幂等。
- 失败：URL 非法、下载失败、超时、非图片、格式不允许或超过 4 MiB 时跳过该图，保留占位符，不使批次失败。图片全部失败且没有可抽取文本时，按空抽取成功处理，水位可推进。
- 关闭：`vision_enabled=false` 时纯图片消息不进入历史；混合消息只保留 `[图片]`；整理不下载图片。

## 相关文档

- 表与行编码：[`architecture.md §4.4`](architecture.md#44-storage-模式)。
- 向量索引与容量：[`vector-index.md §11.5-11.7`](vector-index.md#115-查询算法)。
- 知识库与预算：[`rag.md §10.6-10.7`](rag.md#106-检索与融合)。
- 命令输出：[`admin.md §14.3`](admin.md#143-子命令与输出)。
- 图片选择与多模态渲染：[`conversation.md §8.3`](conversation.md#83-上下文渲染)。
- 视觉 Chat 请求：[`llm.md §12.1`](llm.md#121-chat-completions)。
- 配置键：[`configuration.md §13.1`](configuration.md#131-权威配置键表)。
- 测试矩阵：[`testing.md §15.2`](testing.md#152-单元测试矩阵)。
