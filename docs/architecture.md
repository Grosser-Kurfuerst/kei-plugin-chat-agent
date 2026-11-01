# chat-agent 架构

覆盖：插件定位、边界、数据流、生命周期、并发、状态、Storage 与表存储模式、文件职责与 kei 契约。不覆盖：具体算法细节（见 [`participation.md`](participation.md)、[`memory.md`](memory.md)、[`rag.md`](rag.md)、[`vector-index.md`](vector-index.md)、[`llm.md`](llm.md)）、命令输出（见 [`admin.md`](admin.md)）、配置键默认值（见 [`configuration.md §13.1`](configuration.md#131-权威配置键表)）。

## 1. 项目定位与边界

### 1.1 做什么

`chat-agent` 是 kei 的进程内插件，处理 `bot.EventMessage` 中的群聊与私聊消息，提供：

- 群聊/私聊名单控制，四种模式：`off`、`open`、`whitelist`、`blacklist`。
- 每会话有界的内存历史，按条数、字符数、空闲时间与 LRU 清理。
- 基于内置向量索引的长期记忆：定时抽取、合并、衰减、淘汰、检索与提示词注入。
- 基于内置向量索引的知识库 RAG：URL/文本导入、抓取、分块、嵌入、检索与注入。
- 可选的图片识别与图片记忆：`vision_enabled=true` 时从 `SegImage` 下载图片并作为多模态输入交给视觉 LLM；识别出的文本可由记忆整理写入长期记忆。
- 两个后台任务：记忆整理与消息缓存清理。
- 插件级超级管理员命令，与宿主核心管理员命令并存。

### 1.2 与 kei 的关系

- 只消费 `PluginContext`：通过 `BotAPI` 收发消息、读写 `Storage`，并通过 `PluginContext.Tables` 使用表存储。
- 不修改 kei 核心，不 import `internal` 包，不依赖平台适配器实现。
- 群聊与私聊分别注册消息规则；命令注册为独立规则。
- 平台差异只使用 `pkg/bot` 与 `pkg/message` 的公开原语。

### 1.3 与 kei-plugin-agent 的关系

`kei-plugin-agent`（persona 插件）是独立仓库，与本插件没有代码依赖，也不共享 Storage 键、配置或运行状态。本插件只借鉴其“消息规则 + 批处理 + 后台生成”的装配方式。

### 1.4 不做什么

不支持外部向量数据库（Qdrant、Milvus、pgvector）；不做平台协议实现、本地文件导入、图片生成、图片发送、图片向量检索、流式输出、MCP/工具调用、LLM SDK、kei 核心改动；只处理 `bot.MessageGroup` 与 `bot.MessagePrivate`，不处理 `bot.MessageChannel`。`vision_enabled=false` 时图片只映射为 `[图片]` 占位符；开启后图片仅以视觉 LLM 的识别结果（文本）进入长期记忆，原始图片不写入 Storage。

### 1.5 当前交付物

当前仓库包含 `go.mod`、实现源码、单元测试与 mock 适配器端到端测试。实现状态只在 [`roadmap.md §16.1`](roadmap.md#161-现状) 记录。

## 2. 架构与数据流

### 2.1 入站数据流

```text
平台 Adapter
   │ EventMessage (group/private)
   ▼
kei EventBus ──► Engine ──► Router
                              │
              ┌───────────────┴────────────────┐
              ▼                                ▼
      group handler                     private handler
              │                                │
              └──────────────┬─────────────────┘
                             ▼
              过滤 / 名单 / 会话开关 / 历史记录
                             ▼
                  寻址判定 / 随机决策
                             ▼
                     批处理定时器（每会话）
                             ▼
                  后台生成协程（全局信号量）
                             ▼
            图片下载与多模态内容组装（vision_enabled）
                             ▼
                 RAG 检索（记忆 + 知识库）
                             ▼
                     Chat Completions (LLM)
                             ▼
                  清洗 / 去重 / 截断 / 引用构造
                             ▼
                       BotAPI.Send
```

### 2.2 两条路径的边界

- Handler 路径只做内存判断与状态更新，毫秒级返回；不做网络、LLM、Embedding 或 Storage 同步读取。
- 后台路径承担批处理等待、RAG、LLM、发送、记忆整理、知识库导入与清理。
- `vision_enabled=true` 时图片下载与多模态组装在生成/整理后台路径执行；Handler 只记录 `SegImage` 的 `KeyURL`，不发起下载。
- Handler 阻塞会拖慢同一会话并受宿主 `RuleTimeout`/`EventTimeout` 约束；因此所有慢操作必须离开 Handler。
- 名单拒绝发生在建会话状态之前：被拒绝的消息不产生会话、不进历史、不触发任何后台任务。

### 2.3 后台组件

| 组件 | 数量 | 职责 |
|---|---|---|
| 批处理定时器 | 每会话最多 1 个 | 合并短时间内的连续消息，触发一次生成 |
| 生成协程 | 每会话最多 1 个，全局受 `limits_max_concurrent` 限制 | 图片下载、RAG、LLM、清洗、发送 |
| 图片下载与组装 | 随生成/整理协程，单批最多 4 张 | 从 `SegImage.KeyURL` 下载、SSRF 校验、大小/格式限制、data URL 编码；失败降级 `[图片]` |
| Embedding 工作协程 | 1 个，队列容量 64 | 记忆整理与知识库导入的批量嵌入 |
| Storage 单写协程 | 1 个 | 串行执行全部 KV 与 `vector` 表行写入 |
| 记忆整理任务 | 1 个 | 抽取、合并、衰减、容量淘汰 |
| 缓存清理任务 | 1 个 | 历史裁剪、会话过期、LRU 淘汰 |
| 知识库任务 | 1 个 | 抓取、提取、分块、嵌入、文档切换 |
| 定时 ticker | 2 个 | 记忆整理与缓存清理 |

聊天查询的 embedding 在生成协程内同步调用，不经过后台 Embedding 队列，但同样受全局生成信号量约束。后台队列满时任务返回 `busy` 或可重试失败：记忆不推进水位，知识库不写半成品。

### 2.4 组件职责

| 组件 | 输入 | 输出 | 失败语义 |
|---|---|---|---|
| `policy` | 配置、`policy` 键、命令 | 允许/拒绝 | 非法覆盖回落配置并 warn |
| `history` | 允许的消息、机器人回复 | 有界历史、上下文 | 裁剪时记录 `lostSeq` |
| `decision` | 事件、名单、会话状态 | 批处理计划、reason | 拒绝只记日志 |
| `prompt` | 历史、记忆块、知识库块、图片 | Chat messages（字符串或多模态 parts） | 渲染失败使用空块；图片缺失保留占位符 |
| `vision` | `SegImage` URL、历史图片 | data URL、多模态 parts | 单图失败保留占位符，不阻塞文本 |
| `rag` | 查询文本、两个 collection | 注入块、`rag_status` | 降级为不注入 |
| `memory` | 未整理历史、查询向量 | 记忆记录、检索结果 | 保留水位，下轮重试 |
| `knowledge` | URL/文本 | 文档与分块行 | 旧文档保留，未引用块由启动回收 |
| `vector` | 表行、`meta` | 内存索引、top-k | 跳过坏行并 warn |
| `scheduler` | 两个 interval、手动命令 | 任务触发与状态 | `job_busy` 跳过 |
| `commands` | 命令事件 | 同步输出、异步任务 | 固定错误文案 |
| `storage` | 逻辑键、值、TTL、表行 | KV 与表持久化、单写队列 | warn 与计数，不 panic |

## 3. 生命周期与并发模型

### 3.1 生命周期

#### Setup

1. 从核心接收 `PluginContext` 并保存在插件实例上。
2. 调用 `loadConfig` 读取并校验全部配置；任何校验错误返回 `chat-agent: 配置错误 <key>=<值>: <原因>`，插件不装配。
3. 检查 `PluginContext.HTTPClient != nil`；为 nil 时返回 `chat-agent: 需要 network 权限`，因为 Chat、Embedding 与图片下载都依赖 HTTP。非 nil 时从宿主 transport 派生受限图片下载客户端（`vision.go`），不修改宿主客户端。
4. 检查 `PluginContext.Tables != nil`；`memory_enabled || rag_enabled` 时为 nil 返回 `chat-agent: 需要 TableStorage 支持`，插件不装配。两个开关都关闭时不要求表能力，插件只用 KV 运行聊天。
5. 构造 runtime：会话表、名单、管理员、向量索引、写队列、任务状态、图片下载客户端。
6. 注册 4 条规则：

```go
reg.OnEvent(bot.EventMessage, p.handleGroupMessage,
    bot.WithKind(bot.MessageGroup), bot.WithPriority(0), bot.WithID("chat-agent:group"))
reg.OnEvent(bot.EventMessage, p.handlePrivateMessage,
    bot.WithKind(bot.MessagePrivate), bot.WithPriority(0), bot.WithID("chat-agent:private"))
reg.OnCommand("chat", p.handleCommand,
    bot.WithPriority(100), bot.WithID("chat-agent:admin"))
reg.OnCommand("chat-admin", p.handleCommand,
    bot.WithAdmin(), bot.WithPriority(100), bot.WithID("chat-agent:core-admin"))
```

`/chat` 不带 `WithAdmin()`，由插件在 Handler 内校验 `super_admins`；`/chat-admin` 带 `WithAdmin()`，由宿主 Auth 校验。两条命令是不同规则，因此同时具备两种身份的用户只执行一次。

#### Start

1. 保存插件级 ctx：`context.WithCancel(context.WithoutCancel(ctx))`。
2. 同步加载 `policy` 与 `admins`；失败回落配置并 warn。
3. 会话覆盖 `session:<hash>` 按首条消息懒加载，不在 Start 全量扫描。
4. 向量功能开启（`memory_enabled || rag_enabled`）时：
   1. 用 `storage_timeout=2s` 调 `CreateTable(ctx, "vector")`；`bot.ErrTableExists` 视为成功，其它错误返回 `chat-agent: 存储表初始化失败: <err>`，插件不启动。
   2. 用 `table_load_timeout=30s` 调 `ListRows(ctx, "vector")`；错误返回 `chat-agent: 存储表加载失败: <err>`。
5. 启动 Storage 单写协程与 Embedding 工作协程；置 `vector.state=loading`，异步解析行、回收未引用行、构造内存索引，完成后置 `ready`。
6. 索引 `ready` 后启动两个 ticker；`jobs_run_on_start=true` 时此时各触发一次整理与清理。
7. 启动异步构建后 `Start` 返回。加载完成前聊天可运行；RAG 最多等 `vector_ready_timeout=2s`，仍未就绪则跳过检索并记 `rag_status=loading`。
8. `memory_enabled=false && rag_enabled=false` 时跳过建表、加载与构建，`vector.state=disabled`，直接启动两个 ticker；`jobs_run_on_start` 立即生效。

加载细节：

- 解析顺序：`d:` 文档行 → `m:` 记忆行 → `c:` 分块行 → `meta`；用 `vector_load_workers=4` 并发解析。
- 单行 JSON、base64 或维度损坏时跳过并 warn，不阻断其它行。
- 行键前缀未知时跳过并 warn，不删除该行。
- 只有全部 `d:` 行解析成功时才回收未被任何文档 `chunk_ids` 引用的 `c:` 行；任一 `d:` 行非法时本轮跳过回收、保留分块并 warn。
- 文档 `chunk_ids` 引用的分块不存在时 warn，文档与其余分块继续加载。
- `meta` 缺失或非法时按配置与已有行推导，尽力写回；写回失败只 warn。
- 行级错误不使 `Start` 失败；只有 `ListRows` 本身失败才失败。

#### Stop

1. 标记关闭，拒绝新的写穿透、任务与生成排程。
2. 停止两个 ticker。
3. 排空写队列，等待 `writerDone`。
4. 调用插件级 `cancel()`，终止生成、任务、Embedding 与索引构建。
5. 失效所有会话 timer 与 pending，递增 `epoch`。
6. `wg.Wait`，上限 `shutdown_wait_timeout=5s`；超时记录 Error `shutdown_incomplete` 并返回错误。
7. 重复调用 `Stop` 是幂等的，直接返回。

取消后仍在运行的索引构建必须尽快退出：退出前不得把 `state` 置为 `ready`，也不得再提交表行写入或发送消息。

### 3.2 协程与资源上限

| 资源 | 上限 | 说明 |
|---|---|---|
| 每会话批处理 timer | 1 | 新消息重置窗口，不超过 `batch_max_window` |
| 每会话生成协程 | 1 | 生成中再有命中只标记 `pending` |
| 全局生成并发 | `limits_max_concurrent` | 信号量满时丢弃本批，`reason=semaphore_full` |
| Embedding 后台工作协程 | 1 | 队列容量 64；满时任务失败并保留可重试状态 |
| Storage 单写协程 | 1 | 全部 KV 与 `vector` 表行写经此串行 |
| 索引解析协程 | `vector_load_workers=4` | 启动时并发解析 `ListRows` 结果；纳入 `wg` |
| 记忆整理任务 | 1 | try-lock，未结束则 `job_busy` |
| 缓存清理任务 | 1 | try-lock，未结束则 `job_busy` |
| 知识库任务 | 1 | 全局串行，忙碌返回 `busy` |
| 图片下载 | 每批最多 `vision_max_images_per_request=4` 张 | 在生成/整理协程内同步执行；批量超时 `vision_fetch_timeout=10s`；失败降级占位符 |
| ticker | 2 | 记忆整理、缓存清理 |

### 3.3 锁与所有权

| 锁 | 保护对象 | 约束 |
|---|---|---|
| `sessionsMu` | 会话 map、LRU 顺序 | 不在锁内调用 Storage/网络 |
| `session.mu` | 单个会话的全部字段 | 历史、pending、timer、inflight、epoch、计数器 |
| `policyMu` | 名单模式与列表 | 命令写入与 Handler 读取 |
| `adminsMu` | `super_admins` 集合 | 命令写入与校验 |
| `vectorMu` | 内存索引、`meta` 快照、stale 计数 | 查询持读锁，重建持写锁 |
| `writeMu`/`writeCond` | 写队列 | 单写协程与提交者 |
| `jobMu` | 任务 running/last/next | try-lock |
| `semaphore` | 全局生成并发 | channel 信号量 |

禁止在持锁期间做网络、LLM、Embedding 或 Storage 调用。需要跨调用保持一致的字段先拷贝快照，调用后重新加锁校验 `epoch`。

### 3.4 关闭顺序

关闭顺序固定为：

```text
关闭准入 → 停止 ticker → 排空写队列 → cancel() → 失效 timer/pending、递增 epoch → wg.Wait
```

先排空写队列再取消插件级 ctx，是为了保证持久化后端上最后一次名单/策略/管理员覆盖不被取消丢弃。取消后仍在途的生成结果可能被丢弃，但已经提交的写入必须保留。异步索引构建纳入 `wg`：`Stop` 取消后等待其退出，构建不得再置 `ready`、写表行或发送消息。`Stop` 返回后不得再调用 `BotAPI.Send`。

### 3.5 调度器语义

- 两个 ticker：记忆整理 `memory_consolidate_interval`、缓存清理 `history_cleanup_interval`；值为 0 时关闭对应 ticker。
- ticker 在索引 `ready`（或 `disabled`）后启动；`jobs_run_on_start=true` 时此时各触发一次，否则等第一个 interval。
- 每个任务有独立 `running` 标志（try-lock）；上一次未结束时跳过本次并记 `job_busy`，不排队。
- 任务 ctx 派生自插件级 ctx，带各自超时；`Stop` 取消并 `wg.Wait`。
- 手动 `/chat jobs run` 与定时任务共用同一 `running` 标志；命令立即返回“已触发”，完成/失败另发消息。
- 任务状态 `last`/`next` 只在内存，重启后从 Start 重新计算。

### 3.6 日志

固定字段：

| 字段 | 含义 |
|---|---|
| `operation` | `message`、`generate`、`admin_command`、`consolidate`、`cleanup`、`knowledge`、`vector`、`vision`、`storage` |
| `reason` | 决策 reason 或失败原因 |
| `rag_status` | `ok`、`disabled`、`loading`、`error`、`empty` |
| `session` | 会话键的哈希前缀 |
| `kind` | `group`、`private` |
| `platform` | 平台名，原样 |
| `user` | 发送者 ID |
| `channel` | 频道 ID |
| `event_id` | 事件 ID |
| `duration_ms` | 耗时 |
| `error` | 错误字符串 |

`plugin=chat-agent` 由核心绑定。禁止记录 API key；只有 `debug_prompts=true` 才记录完整请求/响应提示词。`operation=vision` 只记录 `reason`、`host`、`bytes`，不记录完整 URL 或 data URL；`debug_prompts=true` 时图片折叠为 `[image <mime> <bytes>B]`。

## 4. 状态与持久化

### 4.1 sessionState

| 字段 | 类型 | 含义 |
|---|---|---|
| `key` | string | 内存会话键 |
| `kind` | string | `group` 或 `private` |
| `platform` | string | 平台名 |
| `botID` | string | 机器人 ID |
| `channelID` | string | 群聊频道 ID；私聊为空 |
| `peerUserID` | string | 私聊对端用户 ID；群聊为空 |
| `history` | `[]Turn` | 有界历史，按 `seq` 升序 |
| `lastActivity` | time.Time | 最近一条允许消息或机器人回复时间 |
| `disabled` | bool | `/chat off` 覆盖 |
| `seq` | uint64 | 下一条历史的序号 |
| `consolidatedSeq` | uint64 | 已被记忆整理消费到的位置 |
| `lostSeq` | uint64 | 因裁剪丢失的未整理位置上界 |
| `epoch` | uint64 | 会话代次，删除/重置时递增 |
| `timer` | `*time.Timer` | 批处理定时器 |
| `firstAt` | time.Time | 当前批次第一条消息的时间 |
| `pending` | `[]Turn` | 待生成队列：等待定时器或生成期间到达的消息 |
| `inflight` | bool | 是否有生成在途 |
| `lastReplyAt` | time.Time | 最近一次机器人回复时间 |
| `hourly` | `[]time.Time` | 最近一小时的随机插话时间 |
| `recentReplyIDs` | `[]string` | 最近 128 条机器人发送的消息 ID |
| `recentReplyTexts` | `[]replyFingerprint` | 最近 5 条机器人回复文本与时间 |
| `recentSenders` | `[]senderMark` | 最近 `random_activity_window` 内的非机器人发送者 |

`lastSeq` 指会话的 `seq`（下一条历史的序号）；`consolidatedSeq >= lastSeq` 表示没有未整理消息。`pending` 同时承载“等待定时器的当前批次”和“生成期间到达的下一批”，取批时整体移出。

### 4.2 会话键

- 群聊内存键：`<platform>:<botID>:<channelID>`。
- 私聊内存键：`<platform>:<botID>:user:<userID>`。
- Storage `session` 键：`SHA-256(platform + "\x00" + botID + "\x00" + channelKey)` 的 64 位小写 hex；私聊 `channelKey = "user:" + userID`，群聊 `channelKey = channelID`。
- 会话键只用于内存查找；持久化只存覆盖字段，不存历史。

### 4.3 历史环与 LRU

- 历史按条数与总 rune 数裁剪，保留最新条目。
- LRU 按 `lastActivity` 排序；有 `inflight` 或 `pending` 的会话不淘汰。
- 被裁剪且 `seq >= consolidatedSeq` 的条目推进 `lostSeq` 并 warn；整理水位只能前进，不能回退。

### 4.4 Storage 模式

持久化分两层：名单、管理员与会话覆盖经 `bot.Storage` 的 KV 逻辑键；记忆、知识库与向量索引经 `bot.TableStorage` 的单一逻辑表 `vector`。框架对逻辑键与逻辑表名都加 `plugin:chat-agent:` 前缀，前缀 18 字节，因此两者上限均为 `191 - 18 = 173` 字节。表行键不带表名前缀，上限 191 字节。KV 的 TTL 为 0 表示不过期；表行没有 TTL。

KV 逻辑键（全部）：

| 逻辑键 | 值 | TTL | 写入者 |
|---|---|---|---|
| `policy` | JSON `{"group_mode","group_list","private_mode","private_list"}` | 0 | 名单命令 |
| `admins` | JSON `{"super_admins":["platform:userID"]}` | 0 | 管理员命令 |
| `session:<sha256hex>` | JSON `{"disabled":bool,"updated_at":RFC3339Nano}` | 0 | `/chat on\|off\|reset` |

`vector` 表（逻辑表名 `vector`；行键不带前缀）：

| 行键 | 值 | 写入者 |
|---|---|---|
| `m:<id32>` | 记忆记录 JSON | 整理/记忆命令 |
| `d:<id32>` | 文档元数据 JSON | 知识库命令 |
| `c:<id32>` | 知识分块 JSON | 知识库命令 |
| `meta` | 索引元数据 JSON | 索引构建与 `vector reindex` |

- `m:`/`d:`/`c:` 后缀是 32 位小写 hex，行键固定 34 字节；`meta` 是 4 字节固定行键。
- 表单次 `ListRows` 返回全部行；没有分页、列 schema、TTL 或跨行事务。
- 表必须由 `CreateTable` 显式创建；`InsertRow` 不会自动建表。
- 索引只从 `vector` 表发现行，不使用清单键或第二张表。

记录形状（字段名逐字固定）：

```json
// m:<id32>
{"id":"...","scope":"channel|user","scope_key":"...","kind":"fact|preference|relationship|commitment|event",
 "text":"...","importance":0.7,"vector":"<base64>","dims":1536,"model":"...",
 "sources":["event-id"],"created_at":"RFC3339Nano","updated_at":"RFC3339Nano",
 "last_used_at":"RFC3339Nano","use_count":0}

// d:<id32>
{"id":"...","source":"https://...|text:...","title":"...","content_sha256":"...",
 "chunk_ids":["..."],"created_at":"RFC3339Nano","updated_at":"RFC3339Nano"}

// c:<id32>
{"id":"...","doc_id":"...","index":0,"text":"...","vector":"<base64>",
 "dims":1536,"model":"...","created_at":"RFC3339Nano"}

// meta
{"version":1,"model":"...","dims":1536}
```

`meta.version` 固定为 1；`model`/`dims` 是索引期望的 Embedding 模型与维度。`meta` 不是清单：没有 ID 数组，不参与容量计数。

约定：

- 时间一律 UTC、`RFC3339Nano`。
- 向量编码：`base64.StdEncoding`（无换行）编码 little-endian `float32` 序列。
- `m`/`d`/`c` 记录内的 `id` 与行键后缀一致，是 32 位小写 hex（SHA-256 前 16 字节）。
- 会话键哈希：`SHA-256(platform + "\x00" + botID + "\x00" + channelKey)` 的 64 位小写 hex；私聊 `channelKey = "user:" + userID`，群聊为频道 ID。
- 记忆 ID = `SHA-256(scope + "\x00" + scope_key + "\x00" + normalized_text)` 前 16 字节。
- 图片识别后的记忆仍是普通 `m:` 行，`text` 是视觉 LLM 输出；表中不存图片二进制、原始 URL 或图片向量。
- 文档 ID = `SHA-256(normalized_source)` 前 16 字节；`normalized_source` 为 URL 原样去首尾空白，或 `text:<正文>`。
- 分块 ID = `SHA-256(doc_id + "\x00" + index + "\x00" + chunk_text)` 前 16 字节。

写入：

- KV 与表行写入经同一单写协程串行执行；`CreateTable` 与 `ListRows` 只在 `Start` 加载阶段直接调用。
- 行写入统一 `putRow`：先 `UpdateRow`；返回 `bot.ErrNotFound` 时改 `InsertRow`；返回 `bot.ErrRowExists` 时重试一次 `UpdateRow`；其它错误返回 `storage_error`。
- 删除行用 `DeleteRow`；行不存在按成功处理。
- 内存索引只在对应行写成功后切换；失败保持旧快照。
- 单次写超时 `storage_timeout=2s`；失败记 warn 与计数，不 panic。
- 表数据持久性随 Storage 后端：`memory` 后端重启后表为空，`sqlite`/`mysql` 保留。

文档行 `d:<id32>` 是知识库的提交点，一致性、崩溃与回收语义见 §4.7。

### 4.5 会话覆盖懒加载

- 首条消息到达时异步读取 `session:<hash>`。
- 生成前最多等待 1s；仍未返回则按默认 `disabled=false` 继续，并 warn。
- 等待期间 Handler 已经把消息写入内存历史，不丢消息。
- 懒加载结果只覆盖 `disabled` 与 `updated_at`；历史与计数器不持久化。

### 4.6 写穿透

- `policy`、`admins`、`session` 与 `vector` 表的全部 `m:`/`d:`/`c:`/`meta` 行走同一写队列。
- 提交顺序即执行顺序；单个写操作带 `storage_timeout=2s` 超时。
- 写失败只记 warn 与计数，不回滚内存状态；重启后以最后一次成功写入为准。
- 命令输出不等待持久化成功，避免 Handler 被慢存储阻塞。
- 行写入用 §4.4 的 `putRow` 语义；破坏性删除用 `DeleteRow`。

### 4.7 表一致性与孤儿回收

- 知识库的提交点是 `d:<id32>` 行：只有被某个合法 `d:` 行 `chunk_ids` 引用的 `c:` 行才进入索引并计数。
- 新增文档：先写全部 `c:` 行，再写 `d:` 行；`d:` 写入前的失败或崩溃只留下未引用块，旧状态不变。
- 更新文档：先写新 `c:` 行，再写新 `d:` 行（提交），最后删旧块；删旧块失败只 warn，旧块成为未引用行。
- 删除文档：先删 `d:` 行（提交移除），再删旧块；删块失败只 warn。
- 表无跨行事务：崩溃最多留下未引用的 `c:` 行，不会产生指向缺失块的可见文档。
- 启动回收：加载完成后，仅在全部 `d:` 行解析成功时删除未被任何文档引用的 `c:` 行；任一 `d:` 行非法时本轮跳过回收并 warn，保留分块以便修复。
- 未引用块不计入容量、不可检索；回收失败只 warn，下次启动重试。
- `meta` 缺失或非法不是启动失败：按配置与已有行推导并尽力重写，重写失败只 warn。
- 修复入口是 `/chat knowledge refresh` 与 `/chat vector reindex`；二者都只处理表中仍存在的行。

### 4.8 重启语义

| 数据 | `memory` 后端 | `sqlite`/`mysql` 后端 |
|---|---|---|
| 短期历史 | 丢失 | 丢失（历史只在内存） |
| `session` 覆盖 | 丢失 | 保留 |
| `policy`/`admins` | 丢失，回落配置 | 保留 |
| 长期记忆（`m:` 行） | 丢失 | 保留 |
| 知识库文档与分块（`d:`/`c:` 行） | 丢失 | 保留 |
| `meta` 行 | 丢失，启动时重新推导 | 保留 |

`Stop` 时未整理的历史直接丢失；无论后端如何都不落盘。宿主必须配置 `storage.type: sqlite` 或 `mysql` 并空导入 `pkg/kei/sql` 才能跨重启保留记忆、知识库与覆盖。

## 5. 目录与文件职责

实现文件：

| 文件 | 职责 |
|---|---|
| `register.go` | `init()` 注册、`Metadata` |
| `plugin.go` | Plugin 结构、`Setup`/`Start`/`Stop`、`wg`、信号量 |
| `config.go` | 全部键读取、默认值、校验、错误文案 |
| `policy.go` | 四模式名单、条目匹配、持久化 |
| `state.go` | `sessionState`、LRU、epoch、批处理 timer、覆盖懒加载 |
| `history.go` | `Turn`、历史环、裁剪、`lostSeq` |
| `decision.go` | 过滤、寻址、随机、批处理、生成调度 |
| `prompt.go` | 模板渲染、历史/本轮渲染、清洗、去重 |
| `llm.go` | Chat Completions 客户端、多模态 content parts、重试、JSON 抽取 |
| `vision.go` | 图片 URL 校验、SSRF 安全下载、大小/格式/重定向上限、data URL 编码 |
| `embedding.go` | Embeddings 客户端、批量、校验（只接收文本） |
| `vector.go` | 表行编码、内存索引、top-k、文档提交与行写入协议 |
| `memory.go` | 抽取、合并、衰减、容量、检索 |
| `knowledge.go` | URL 抓取、HTML 提取、分块、文档管理 |
| `rag.go` | 双库检索、预算、注入 |
| `scheduler.go` | 两个 ticker、任务互斥、手动触发、缓存清理与向量重建任务 |
| `commands.go` | 命令解析、同步输出、异步任务回复、审计 |
| `admin.go` | `super_admins` 集合、匹配与持久化 |
| `storage.go` | KV/表键构造、单写协程、`putRow`/`DeleteRow`、JSON/向量编码 |
| `*_test.go` | 对应单测；`e2e_test.go` mock 适配器端到端 |

## 6. 与 kei 核心的契约对应

| 契约 | 事实 | 插件应对 |
|---|---|---|
| 生命周期 | `PluginContext` 只注入 `Setup`/`Start`/`Stop` | `Setup` 捕获并挂实例 |
| Handler ctx | 只有 `bot.Route`，没有 `PluginContext` | 不依赖 ctx 取配置或 Storage |
| Start ctx | 返回后立即取消 | 用 `context.WithCancel(context.WithoutCancel(ctx))` 派生插件级 ctx |
| 管理员 | 核心只认 `auth.admin_users` + `WithAdmin()` | `/chat` 用插件超管，`/chat-admin` 用 `WithAdmin()` |
| 发送 | `BotAPI.Send` 需要 `send_message` 权限 | 只声明 `send_message`，发送失败不重排 |
| Storage | `Get`/`Set`/`Delete` + TTL，只有 KV | 名单、管理员、会话覆盖用 KV；逻辑键 ≤173 字节 |
| Tables | `PluginContext.Tables` 暴露 `bot.TableStorage`，与 `Storage` 共用 `storage` 权限；可为实现主体自定义的 nil | 表名 ≤173 字节、行键 ≤191 字节、无 TTL/事务/分页；记忆与 RAG 开启时 `Tables` 必须非 nil，否则 `Setup` 报错 |
| 超时 | 宿主有 `RuleTimeout`/`EventTimeout` | Handler 非阻塞；慢操作放后台 |
| 每 bot 白名单 | 核心插件白名单只收窄，不扩大 | 不假设插件能绕过宿主限制 |
| 日志 | 核心绑定插件名 | 字段化日志，不重复 `plugin` 字段 |
| metrics | 核心没有插件 metrics API | 只提供状态命令与结构化日志 |
| 测试导入 | 生产禁止 `internal`；测试可导入 mock 与 `pkg/kei` | 端到端测试用 `adapters/mock` |

`BotAPI` 只有 `Send`、`Reply`、`Logger`、`Storage`；表能力只在 `PluginContext.Tables` 上，`BotAPI` 没有方法取 TableStorage。本插件也不依赖不存在的用户查询、历史拉取、能力查询、定时器或向量 API。

`Metadata` 固定字段：`Version: "v0.1.0"`、`Author: "RandomLemon"`、`Description: "带长期记忆与知识库 RAG 的聊天代理"`。权限只声明 `network`、`storage`、`send_message`，不声明 `*`。

`Metadata.Options` 必须逐字包含以下顶层键，与 [`configuration.md §13.1`](configuration.md#131-权威配置键表) 完全一致：

```go
Options: []string{
    "group_policy",
    "group_list",
    "private_policy",
    "private_list",
    "ignore_bots",
    "respond_to_commands",
    "self_ids",
    "trigger_keywords",
    "trigger_min_chars",
    "super_admins",
    "mention_reply_probability",
    "mention_min_interval",
    "random_enabled",
    "random_probability",
    "random_cooldown",
    "random_max_per_hour",
    "random_min_participants",
    "random_activity_window",
    "random_quiet_hours",
    "random_timezone",
    "batch_window",
    "batch_max_window",
    "limits_max_concurrent",
    "reply_max_chars",
    "reply_mention_sender",
    "reply_quote",
    "reply_dedupe",
    "history_max_messages",
    "history_max_chars",
    "history_max_sessions",
    "history_ttl",
    "history_unconsolidated_ttl",
    "history_cleanup_interval",
    "context_max_messages",
    "context_max_chars",
    "bot_name",
    "system_prompt",
    "llm_base_url",
    "llm_api_key",
    "llm_model",
    "llm_temperature",
    "llm_max_tokens",
    "llm_timeout",
    "llm_max_retries",
    "llm_skip_token",
    "llm_skip_enabled",
    "llm_extra_headers",
    "debug_prompts",
    "vision_enabled",
    "memory_enabled",
    "rag_enabled",
    "embedding_base_url",
    "embedding_api_key",
    "embedding_model",
    "embedding_dimensions",
    "embedding_timeout",
    "embedding_max_retries",
    "embedding_extra_headers",
    "vector_max_records",
    "memory_consolidate_interval",
    "memory_consolidate_max_sessions",
    "memory_consolidate_max_messages",
    "memory_consolidate_timeout",
    "memory_extract_max",
    "memory_extract_min_messages",
    "memory_dedupe_threshold",
    "memory_min_score",
    "memory_top_k",
    "memory_context_max_chars",
    "memory_importance_decay_half_life",
    "memory_min_importance",
    "memory_ttl",
    "rag_context_max_chars",
    "knowledge_top_k",
    "knowledge_min_score",
    "knowledge_context_max_chars",
    "knowledge_chunk_chars",
    "knowledge_chunk_overlap",
    "knowledge_max_bytes",
    "knowledge_allowed_content_types",
    "knowledge_url_allow_private",
    "jobs_run_on_start",
}
```

### 6.1 核心管理员不可读

- 插件没有 API 查询宿主 `auth.admin_users`。
- `/chat` 在 Handler 内校验插件配置与 Storage 覆盖的 `super_admins`。
- `/chat-admin` 使用 `WithAdmin()`，由宿主 Auth 校验；两者是不同命令，不存在同一事件重复执行。
- 同时具备两种身份的用户可用任一命令；核心管理员若要用 `/chat`，必须把自身加入 `super_admins`。
- 宿主未配置 Auth 时 `/chat-admin` 被拒绝，这是宿主行为，插件不提供绕过。

## 相关文档

- 消息过滤、寻址、随机与批处理：[`participation.md`](participation.md) 第 7 章。
- 会话与历史：[`conversation.md`](conversation.md) 第 8 章。
- 长期记忆：[`memory.md`](memory.md) 第 9 章。
- 知识库：[`rag.md`](rag.md) 第 10 章。
- 向量索引：[`vector-index.md`](vector-index.md) 第 11 章。
- 配置键：[`configuration.md §13.1`](configuration.md#131-权威配置键表)。
- 命令：[`admin.md §14.2-14.3`](admin.md#142-命令语法)。
