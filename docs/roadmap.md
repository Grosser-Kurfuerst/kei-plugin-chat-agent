# 路线图

覆盖：当前交付状态、P1-P8 实现阶段、已知缺口、假设与兜底、变更流程。不覆盖：具体设计细节，见前文章节。状态只在本章 §16.1 更新。

## 16. 路线图

### 16.1 现状

**只有文档，没有代码。当前状态：文档完成，代码未开始。**

| 项 | 状态 |
|---|---|
| `AGENTS.md` | 完成 |
| `docs/` 全部文档 | 完成 |
| `go.mod` | 未创建 |
| `.go` 源码 | 未创建 |
| 测试 | 未创建 |
| 根 `README.md` | 未创建 |

- 文档是本仓库当前唯一交付物，也是后续实现的验收基线。
- 实现从 P1 开始，按 §16.2 顺序推进。
- 本阶段不创建脚手架、占位代码或测试桩。

### 16.2 实现阶段

| 阶段 | 交付物 | 验收 |
|---|---|---|
| P1 骨架 | `go.mod`、`register.go`、`config.go`、`plugin.go`、`policy.go`、`state.go`、`history.go`、四条规则 | 编译通过；插件可启动/停止；配置校验全过；名单与历史单测通过 |
| P2 决策与聊天 | `decision.go`、`prompt.go`、`llm.go`、`vision.go`、批处理、发送、reason 与日志 | 消息过滤/寻址/随机/批处理/发送单测通过；图片准入、下载、多模态组装与降级单测通过；固定输出与 reason 逐字一致 |
| P3 Embedding 与向量 | `embedding.go`、`vector.go`、`vector` 表存储、加载、reindex | 索引加载/写入/删除/top-k/容量/reindex 单测通过；`go test -race` 通过 |
| P4 长期记忆 | `memory.go`、抽取、合并、衰减、容量、图片记忆、记忆命令 | 水位、抽取解析、合并、衰减、检索强化、图片分批与图片文本记忆单测通过 |
| P5 知识库 | `knowledge.go`、抓取、HTML 提取、分块、文档命令 | SSRF/重定向/Content-Type/分块/幂等/删除单测通过 |
| P6 RAG 注入 | `rag.go`、双库检索、预算、提示词块、降级 | 注入格式、预算丢弃顺序、未就绪/错误降级单测通过 |
| P7 管理员与调度 | `admin.go`、`scheduler.go`、`commands.go`、两个任务、审计 | 双管理员、最后管理员、持久化、任务互斥、状态输出单测通过 |
| P8 联调 | mock 适配器端到端、竞态/关闭、文档对齐、质量门 | 端到端 8 个场景通过；`go test -race` 通过；文档校验通过 |

统一验收：

- 该阶段代码可编译：`go build ./...`。
- 插件可启动、可停止：mock 宿主加载成功，`Stop` 幂等。
- 对应单测通过：`go test ./...`。
- 竞态通过：`go test -race ./...`。
- 不新增第三方运行时依赖。

P8 另加：

- `adapters/mock` 端到端场景全部通过。
- 生成/Embedding/知识库/写队列关闭场景通过。
- 文档索引、章号、配置键、固定字面量校验通过。

### 16.3 已知缺口

| 缺口 | 影响 | 处理 |
|---|---|---|
| 短期历史只存内存 | 重启与 `Stop` 丢失未整理消息 | 接受；需要长期信息时依赖整理任务 |
| 内置暴力向量索引 | 查询 `O(N·D)`，上限 10k | 超过上限按 §11.7 淘汰或拒绝 |
| TableStorage 依赖 | 自定义 `Storage` 未实现 `TableStorage` 时，记忆/RAG 开启会在 `Setup` 被拒绝 | 关闭两个功能可只用 KV；否则改用内置后端或自实现表能力 |
| 表无事务与 TTL | 知识库以 `d:` 行提交，崩溃留下未引用 `c:` 行 | 启动回收清理；回收失败则暂存到下次启动 |
| 表名/行键上限 | 逻辑表名 ≤173 字节、行键 ≤191 字节 | 当前固定短名称满足；新增表名需重新核算 |
| 知识库只支持 URL/文本 | 不支持本地文件、PDF、JS 页面、登录页 | 由外部先转成文本或可访问 URL |
| HTML 提取为启发式 | 复杂页面正文可能缺失或混入导航 | 用 `title` 与 `add-text` 兜底 |
| 图片记忆只存识别文本 | 原始图片不落盘，不能按图片相似度检索；依赖视觉 LLM 的描述质量 | 接受；需要图片向量时另立多模态 Embedding 设计 |
| 图片 URL 可能过期或不可达 | 整理时无法重新下载，图片只留 `[图片]` 占位符 | 缩短整理间隔或使用长期有效 URL；不缓存图片二进制 |
| 视觉模型能力不可探测 | 配置了非视觉模型时，含图请求在运行期返回 `llm_error` | 启动不探测；按文档配置视觉模型 |
| 无图片生成/流式/工具调用/MCP | 不生成图片，不处理流式、工具调用或 MCP | 边界之外 |
| 无插件级 metrics | 只能看状态命令与结构化日志 | 用日志聚合 |
| 插件读不到核心管理员列表 | `/chat-admin` 必须是独立命令 | 双命令授权模型 |
| 裸 ID 名单条目跨平台匹配 | 多平台部署可能误放行/误拒绝 | 使用 `platform:id` |
| `memory` 后端重启全丢 | 记忆、知识库、覆盖不持久 | 宿主改 `sqlite`/`mysql` |
| 配置不热更新 | 模型/Embedding/任务间隔改动需重启 | 重启宿主 |
| 记忆强化延迟落库 | `Stop` 可能丢失少量强化计数 | 接受；下轮整理落库 |
| 模型/维度变更需 reindex | 额外 Embedding 成本 | 变更后执行 `/chat vector reindex` |
| 任务只在单机运行 | 无分布式协调与多实例共享 | 单实例部署 |

### 16.4 假设与兜底

1. kei 契约以本地 HEAD 为准；开发使用 `replace github.com/RandomLemon/kei => ../kei`。
2. 宿主启用 Auth 时 `/chat-admin` 才可用；未启用时该命令被宿主拒绝。
3. Embedding 服务实现 OpenAI 兼容 `/embeddings`；Chat 服务实现 `/chat/completions`。
4. 若 `pkg/bot` 缺少浮点读取，使用 `Get` + 类型断言，不引入依赖。
5. 若 Storage 逻辑键超限，使用 SHA-256 缩短；现有设计已满足 173 字节上限。表名与行键同理，行键固定 34 字节或 4 字节。
6. 宿主必须配置 `storage.type: sqlite` 或 `mysql` 并空导入 `pkg/kei/sql` 才能跨重启保留数据。
7. 内置 `memory`/`sqlite`/`mysql` 后端都提供 `TableStorage`；自定义 `Storage` 必须自行实现表能力，否则记忆/RAG 开启时 `Setup` 被拒绝。
8. 不提供旧 KV 索引键或旧索引清单的迁移；仓库当前没有实现与线上数据，实现直接使用 `vector` 表。
9. 平台适配器必须提供 `Sender.ID`、`Channel.ID`、消息段与消息 ID；缺失字段按空值处理并 warn。图片消息必须提供 `SegImage` 的 `KeyURL`，且该 URL 可被宿主 `HTTPClient` 访问；`KeyFile` 不支持。
10. 配置环境变量由宿主解析；插件只读 `PluginContext.Config`。
11. 文档正文中文；标识符、命令、错误文案保持原样。
12. 本阶段没有代码，文档冲突时以本文档集内部一致为准；一旦有代码，以代码为准并同步文档。
13. `vision_enabled=true` 时 `llm_model` 必须接受 OpenAI 多模态 `content` 数组；Embeddings 仍只接收识别文本，不需要图片输入。

### 16.5 变更流程

- 先改文档，再改代码；文档与代码同一次变更内保持一致。
- 配置键增删必须同步：[`configuration.md §13.1`](configuration.md#131-权威配置键表)、§13.6 清单、[`architecture.md §6`](architecture.md#6-与-kei-核心的契约对应) 的 `Metadata.Options`、示例配置、§13.4 校验。
- 命令或输出变更必须同步 [`admin.md §14.2-14.3`](admin.md#142-命令语法)；异步失败原因同步 §14.5。
- reason 词表变更必须同步 [`participation.md §7.7`](participation.md#77-决策日志与-reason-词表) 与 [`testing.md §15.2`](testing.md#152-单元测试矩阵)。
- 表名、行键或行 JSON 形状变更必须同步 [`architecture.md §4.4`](architecture.md#44-storage-模式)、[`vector-index.md §11.2`](vector-index.md#112-数据结构)、[`rag.md §10.5`](rag.md#105-索引与更新)、[`memory.md §9.1`](memory.md#91-数据模型)、[`testing.md §15.2`](testing.md#152-单元测试矩阵)。
- 存储键或 JSON 形状变更必须同步 [`architecture.md §4.4`](architecture.md#44-storage-模式)、[`vector-index.md §11.2`](vector-index.md#112-数据结构) 与相关算法文档。
- 交付状态只在 §16.1 更新；其它文档不写实现状态。
- commit message 与 tag 使用英文。

## 相关文档

- 文档索引：`README.md` §1。
- 架构与生命周期：[`architecture.md`](architecture.md) 第 1-6 章。
- 配置键：[`configuration.md §13.1`](configuration.md#131-权威配置键表)。
- 命令：[`admin.md §14.2-14.3`](admin.md#142-命令语法)。
- 测试与完成定义：[`testing.md`](testing.md) 第 15 章。
