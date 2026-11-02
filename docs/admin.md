# 管理员与命令

覆盖：插件超级管理员与宿主核心管理员两套授权、完整命令语法、固定输出、持久化、异步任务、审计与恢复。不覆盖：消息决策（见 [`participation.md`](participation.md) 第 7 章）、记忆/知识库算法（见 [`memory.md`](memory.md) 第 9 章、[`rag.md`](rag.md) 第 10 章）、配置键默认值（见 [`configuration.md §13.1`](configuration.md#131-权威配置键表)）。

## 14. 管理员与命令

### 14.1 授权模型

插件超级管理员：

- 配置 `super_admins` 是种子；Storage `admins` 键存在时其 `super_admins` 数组全量覆盖配置。
- 启动时同步加载；加载失败回落配置并 warn。
- 每项必须是 `platform:userID`；匹配方式是 `ev.Platform + ":" + ev.Sender.ID` 精确比较，区分大小写，不支持通配符。
- 运行时 `/chat admins add|del` 修改内存集合并写穿透 `admins` 键。
- `/chat admins reset` 删除 `admins` 键，恢复配置值。
- 删除操作必须保证至少剩一个有效超级管理员，否则输出 `chat-agent: 不能删除最后一个超级管理员`。
- 有效集合为空时 warn：无超级管理员，只能由核心管理员通过 `/chat-admin` 恢复。

核心管理员：

- `/chat-admin` 注册时带 `bot.WithAdmin()`，由宿主 `auth.admin_users` 校验。
- 插件没有 API 查询 `auth.admin_users`，因此不能把核心管理员合并进 `/chat` 的校验；契约见 [`architecture.md §6.1`](architecture.md#61-核心管理员不可读)。
- `/chat` 与 `/chat-admin` 是不同命令，各自只执行一次；同时具备两种身份的用户用任一命令即可。
- 核心管理员若要使用 `/chat`，必须把自身加入 `super_admins` 或通过 `/chat-admin admins add` 添加。
- 宿主未配置 Auth 时 `/chat-admin` 被拒绝，这是宿主行为，插件不提供绕过。

命令消息：

- `/chat` 与 `/chat-admin` 不进入历史、不调用聊天 LLM、不受名单策略与会话开关影响。
- 权限校验在 Handler 内同步完成；异步任务在触发时已校验，后台不再检查权限。

### 14.2 命令语法

```text
/chat help
/chat status
/chat on | off | reset
/chat policy [group|private [off|open|whitelist|blacklist]]
/chat policy reset
/chat list [group|private [add|del <id>]]
/chat admins [list|add <platform:userID>|del <platform:userID>|reset]
/chat memory [status|search <query>|add <text>|del <id>|reset confirm]
/chat knowledge [list|add <url> [title]|add-text <text>|del <id>|refresh [id]]
/chat jobs [status|run consolidate|run cleanup]
/chat vector [status|reindex]
/chat-admin <同一子命令集>
```

规则：

- `/chat` 单独执行或未知子命令时输出用法；已知子命令缺少必需参数时输出参数错误。
- `/chat-admin` 使用同一子命令集，唯一区别是授权来源。
- 以下省略等价：
  - `/chat admins` = `/chat admins list`
  - `/chat memory` = `/chat memory status`
  - `/chat knowledge` = `/chat knowledge list`
  - `/chat jobs` = `/chat jobs status`
  - `/chat vector` = `/chat vector status`
  - `/chat policy` 显示两种模式；名单用 `/chat list`
  - `/chat list` 显示两个列表
- `policy` 设置必须写成 `/chat policy <group|private> <mode>`；`/chat policy <mode>` 是参数错误。
- `list add|del` 必须写成 `/chat list <group|private> add|del <id>`；缺少类型是参数错误。
- `memory search` 的查询、`memory add` 的文本、`knowledge add-text` 的文本都取剩余整段参数。
- `knowledge add` 的 `title` 是第二个位置参数，可省略。

### 14.3 子命令与输出

固定输出前缀与格式：

| 场景 | 逐字输出 |
|---|---|
| 无权执行 | `chat-agent: 无权执行该命令` |
| 用法 | `chat-agent: 用法: /chat <status\|on\|off\|reset\|policy\|list\|admins\|memory\|knowledge\|jobs\|vector\|help>` |
| 参数错误 | `chat-agent: 参数错误: <detail>` |
| 忙碌 | memory/vector：`chat-agent: 忙碌: <memory\|vector>`；knowledge：`knowledge: 失败 busy`；jobs：`jobs: 失败 <name>: job_busy` |
| 功能未启用 | `chat-agent: 记忆功能未启用` / `chat-agent: 知识库功能未启用` |
| 向量未就绪 | `chat-agent: 向量索引未就绪` |
| 未找到 | `chat-agent: 未找到: <id>` |
| 最后管理员 | `chat-agent: 不能删除最后一个超级管理员` |
| 状态行 | `chat-agent: enabled=<true\|false> kind=<group\|private> policy=<mode> history=<n> vector=<ready\|loading\|disabled> memory=<n> knowledge=<docs>/<chunks>` |
| 名单查看 | `policy: group=<mode> private=<mode>`；`list group: <id,id\|(empty)>`；`list private: ...` |
| 名单修改 | `list <group\|private>: 已添加 <id>` / `已删除 <id>`；`policy: group=<mode>` / `policy: private=<mode>`；`policy: 已重置` |
| 管理员 | `admins: <id,id\|(empty)>`；`admins: 已添加 <id>`；`admins: 已删除 <id>`；`admins: 已重置` |
| 会话开关 | `chat-agent: 本会话已启用` / `本会话已停用` / `本会话已重置` |
| 记忆状态 | `memory: records=<n> stale=<n> pending_sessions=<n> last=<RFC3339\|never>` |
| 记忆搜索 | 开始 `memory search: 已开始`；完成 `memory search: <n> 条结果`，随后每行 `<id> <score:.3f> <kind> <text>`；无结果 `memory search: 无结果`；失败 `memory search: 失败 <reason>` |
| 记忆写入 | `memory: 已接受`；完成 `memory: 完成 <id>`；失败 `memory: 失败 <reason>` |
| 知识库列表 | `knowledge: docs=<n> chunks=<n>`；每行 `<docID> <title> chunks=<n> updated=<RFC3339>`；空 `knowledge: (empty)` |
| 知识库任务 | `knowledge: 已开始 <url\|text\|del\|refresh>`；完成 `knowledge: 完成 doc=<id> chunks=<n> result=<added\|updated\|unchanged\|deleted>`；失败 `knowledge: 失败 <reason>` |
| 任务状态 | `jobs: consolidate=<idle\|running> last=<RFC3339\|never> next=<RFC3339\|never> cleanup=<idle\|running> last=<...> next=<...>` |
| 任务触发 | `jobs: 已触发 <consolidate\|cleanup>`；完成 `jobs: 完成 <name>: <summary>`；失败 `jobs: 失败 <name>: <reason>` |
| 向量状态 | `vector: state=<ready\|loading\|disabled> model=<model> dims=<n> memory=<n> knowledge=<n> stale=<n>` |
| 向量重建 | `vector: 已开始重建`；完成 `vector: 完成 reindexed=<n>` |

#### 14.3.1 help 与 status

- `/chat help` 输出用法行；不输出其它内容。
- `/chat status` 输出状态行：
  - `enabled` = 当前会话 `!disabled`。
  - `kind` = 当前会话类型。
  - `policy` = 当前会话类型对应的有效模式（群聊 `group_mode`，私聊 `private_mode`）。
  - `history` = 当前会话历史条数。
  - `vector` = `ready`/`loading`/`disabled`。
  - `memory` = 记忆记录数。
  - `knowledge` = 文档数/分块数。

#### 14.3.2 on/off/reset

- `/chat off`：设置当前会话 `disabled=true`，写 `session:<hash>`，输出 `chat-agent: 本会话已停用`。
- `/chat on`：设置 `disabled=false`，输出 `chat-agent: 本会话已启用`。
- `/chat reset`：删除 `session:<hash>` 覆盖，恢复 `disabled=false`，输出 `chat-agent: 本会话已重置`。
- 三个命令只影响当前会话，不影响名单、记忆或知识库。
- 会话尚未创建时，按命令事件构造会话键并建立最小状态；`on`/`off`/`reset` 不要求已有历史。
- 命令本身在会话关闭时仍可执行。

#### 14.3.3 policy 与 list

- `/chat policy` 输出 `policy: group=<mode> private=<mode>`。
- `/chat policy <group|private> <mode>` 设置模式，输出 `policy: <group|private>=<mode>`。
- `/chat policy reset` 删除 `policy` 覆盖，输出 `policy: 已重置`。
- `/chat list` 输出两行：`list group: <id,id|(empty)>` 与 `list private: <id,id|(empty)>`。
- `/chat list <group|private>` 输出对应一行。
- `/chat list <group|private> add <id>` 输出 `list <group|private>: 已添加 <id>`。
- `/chat list <group|private> del <id>` 输出 `list <group|private>: 已删除 <id>`。
- 条目校验与去重见 [`participation.md §7.8`](participation.md#78-名单策略)。

#### 14.3.4 admins

- `/chat admins` 输出 `admins: <id,id|(empty)>`。
- `/chat admins add <platform:userID>` 输出 `admins: 已添加 <id>`。
- `/chat admins del <platform:userID>` 输出 `admins: 已删除 <id>`；最后管理员时输出固定拒绝文案，不修改。
- `/chat admins reset` 输出 `admins: 已重置`，删除 Storage 覆盖并恢复配置。
- 添加已存在条目视为成功，输出 `admins: 已添加 <id>`，不重复存储。
- 删除不存在条目视为成功，输出 `admins: 已删除 <id>`。

#### 14.3.5 memory

- `/chat memory status` 见 [`memory.md §9.7`](memory.md#97-管理命令)；`records` 计全部 `m:` 行（含无法解析的行），`stale` 是可解析且模型/维度不匹配的行。
- `/chat memory search <query>` 异步检索：先输出 `memory search: 已开始`，完成后发送结果；失败输出 `memory search: 失败 <reason>`；`memory_enabled=false` 时输出未启用。查询 embedding 超出 `embedding_timeout` 或任务忙碌时分别输出 `timeout`、`chat-agent: 忙碌: memory`。
- `/chat memory add <text>` 输出 `memory: 已接受`，后台完成后输出 `memory: 完成 <id>` 或 `memory: 失败 <reason>`。
- `/chat memory del <id>` 未找到时输出 `chat-agent: 未找到: <id>`；找到时先输出 `memory: 已接受`，完成后输出 `memory: 完成 <id>`。
- `/chat memory reset confirm` 输出 `memory: 已接受`，完成后输出 `memory: 完成 reset`；缺少 `confirm` 是参数错误。
- 记忆任务互斥时输出 `chat-agent: 忙碌: memory`。
- `vector.state=loading` 时全部记忆子命令输出 `chat-agent: 向量索引未就绪`；`memory_enabled=false` 的未启用文案优先。

#### 14.3.6 knowledge

- `/chat knowledge list` 输出文档总数、分块总数与每个文档一行；无文档输出 `knowledge: (empty)`。
- `/chat knowledge add <url> [title]` 输出 `knowledge: 已开始 <url>`。
- `/chat knowledge add-text <text>` 输出 `knowledge: 已开始 text`。
- `/chat knowledge del <id>` 与 `/chat knowledge refresh <id>` 未找到时输出 `chat-agent: 未找到: <id>`；找到时先输出对应的 `knowledge: 已开始 del` 或 `knowledge: 已开始 refresh`。
- `/chat knowledge refresh [id]` 输出 `knowledge: 已开始 refresh`。
- 完成输出 `knowledge: 完成 doc=<id> chunks=<n> result=<...>`；失败输出 `knowledge: 失败 <reason>`。
- 任务锁被占用时输出 `knowledge: 失败 busy`。
- `rag_enabled=false` 时输出 `chat-agent: 知识库功能未启用`。
- `del` 与 `refresh <id>` 的存在性检查查内存 docs map（由 `d:` 行加载）。
- `vector.state=loading` 时全部知识库子命令输出 `chat-agent: 向量索引未就绪`；未启用文案优先。

#### 14.3.7 jobs

- `/chat jobs status` 输出 `jobs: consolidate=<idle|running> last=<RFC3339|never> next=<RFC3339|never> cleanup=<idle|running> last=<...> next=<...>`。
- `/chat jobs run consolidate` 输出 `jobs: 已触发 consolidate`。
- `/chat jobs run cleanup` 输出 `jobs: 已触发 cleanup`。
- 完成输出 `jobs: 完成 <name>: <summary>`；失败输出 `jobs: 失败 <name>: <reason>`。
- 任务正在运行时，触发命令立即返回 `jobs: 失败 <name>: job_busy`，不排队。
- `vector.state=loading` 时 `jobs run consolidate` 输出 `chat-agent: 向量索引未就绪`；`jobs run cleanup` 不依赖索引，允许执行。
- `last`/`next` 只在内存；重启后从 Start 重新计算。

#### 14.3.8 vector

- `/chat vector status` 输出 `vector: state=<ready|loading|disabled> model=<model> dims=<n> memory=<n> knowledge=<n> stale=<n>`。
- `model`/`dims` 取 `meta` 的期望值；`meta` 缺失或非法时报配置值，维度为 0。
- `memory`/`knowledge` 只计非 stale、可检索行；`stale` 计全部 stale 记忆行与分块行。
- `state=loading` 时其余字段报告已解析部分；未完成前 RAG 最多等 2s。
- `/chat vector reindex` 输出 `vector: 已开始重建`；完成输出 `vector: 完成 reindexed=<n>`。
- 无 stale 时完成输出 `vector: 完成 reindexed=0`。
- `state=loading` 或 `state=disabled` 时执行 `reindex` 输出 `chat-agent: 向量索引未就绪`，不启动任务。
- 已有 reindex 时输出 `chat-agent: 忙碌: vector`。

### 14.4 持久化与优先级

KV 逻辑键：

| 逻辑键 | JSON | 写入命令 | 重置命令 | 生效顺序 |
|---|---|---|---|---|
| `policy` | `{"group_mode","group_list","private_mode","private_list"}` | `policy`/`list` | `policy reset` | Storage 覆盖 > 配置 |
| `admins` | `{"super_admins":["platform:userID"]}` | `admins add/del` | `admins reset` | Storage 覆盖 > 配置 |
| `session:<sha256hex>` | `{"disabled":bool,"updated_at":RFC3339Nano}` | `on`/`off` | `reset` | Storage 覆盖 > 默认 `false` |

`vector` 表行（逻辑表名 `vector`，由 `bot.TableStorage` 提供）：

| 行键 | 内容 | 写入命令 | 删除命令 |
|---|---|---|---|
| `m:<id32>` | 记忆记录 | `memory add`、整理、强化落库 | `memory del`、`memory reset confirm`、衰减/TTL、容量淘汰 |
| `d:<id32>` | 文档元数据 | `knowledge add`、`add-text`、`refresh` | `knowledge del` |
| `c:<id32>` | 知识分块 | `knowledge add`、`add-text`、`refresh` | `knowledge del`、文档更新后删旧块、启动回收 |
| `meta` | 索引元数据 | 索引构建、`vector reindex` | 无需删除 |

规则：

- 启动时同步加载 `policy` 与 `admins`；`session` 按首条消息懒加载；`vector` 表行在 `Start` 用 `ListRows` 异步加载。
- 所有 KV 与表行写入经同一单写协程；命令在内存更新后提交写入并立即输出成功，不等待持久化。
- 写失败只记 warn；重启后以最后一次成功写入为准。
- `reset` 是删除键，不是写入空值；删除后恢复配置默认。
- `policy` JSON 损坏或字段非法时回落配置；`admins` JSON 损坏时回落配置。
- `session` 只持久化 `disabled`，不持久化历史。
- `m:`/`d:`/`c:` 行走 `putRow`/`DeleteRow`；`d:` 行是知识库提交点，语义见 [`architecture.md §4.7`](architecture.md#47-表一致性与孤儿回收)。
- `memory` 后端重启丢失 KV 覆盖与 `vector` 表数据；`sqlite`/`mysql` 两者都保留。

### 14.5 异步任务

| 任务 | 触发输出 | 完成输出 | 失败输出 |
|---|---|---|---|
| 记忆搜索 | `memory search: 已开始` | `memory search: <n> 条结果` / `无结果` | `memory search: 失败 <reason>` |
| 记忆添加 | `memory: 已接受` | `memory: 完成 <id>` | `memory: 失败 <reason>` |
| 记忆删除 | `memory: 已接受` | `memory: 完成 <id>` | `memory: 失败 <reason>` |
| 记忆重置 | `memory: 已接受` | `memory: 完成 reset` | `memory: 失败 <reason>` |
| 知识库导入/删除/刷新 | `knowledge: 已开始 <url\|text\|del\|refresh>` | `knowledge: 完成 doc=<id> chunks=<n> result=<...>` | `knowledge: 失败 <reason>` |
| 任务触发 | `jobs: 已触发 <consolidate\|cleanup>` | `jobs: 完成 <name>: <summary>` | `jobs: 失败 <name>: <reason>` |
| 向量重建 | `vector: 已开始重建` | `vector: 完成 reindexed=<n>` | 仍发完成消息，`reindexed` 为成功数；失败记日志，记录保持 stale |

异步失败 reason 词表：

```text
busy, timeout, fetch_error, url_rejected, unsupported_type, too_large,
empty_content, chunk_limit, embedding_error, storage_error, vector_full,
invalid_text, not_found, internal_error
```

`jobs` 失败只用 `job_busy`、`timeout`、`internal_error`。

规则：

- 任务串行：记忆整理、记忆命令与向量重建共享记忆变更互斥；知识库任务独立；同类/相关任务忙碌时立即失败，不排队。清理可与这些任务并行，但共享单写协程与 Embedding 队列。
- 完成/失败消息发回触发命令的会话；发送失败只记日志，不重试。
- 触发时已完成授权；后台任务不再检查权限。
- 任务失败不修改已提交的数据；记忆水位与已提交的 `m:`/`d:` 行保持旧状态。
- 异步任务不因会话被关闭而取消；`Stop` 才取消。

### 14.6 审计与安全

每条命令记录：

```text
operation=admin_command command=chat|chat-admin subcommand=<name> platform=<platform> user=<userID> result=<ok|denied|error> duration_ms=<n>
```

- `result=denied` 用于非超级管理员执行 `/chat`。
- `/chat-admin` 的拒绝由宿主发生，插件不记录。
- 不记录命令参数中的完整文本、URL 查询串中的敏感信息或 API key。
- 命令消息不进入历史，不参与记忆抽取。
- 命令绕过名单策略、会话开关、`trigger_min_chars` 与批处理。
- 审计只写日志，不写 Storage。

### 14.7 失败与恢复

| 场景 | 可见结果 | 恢复 |
|---|---|---|
| KV 或表后端不可用 | 命令仍生效于内存，写失败只记 warn | 修复后端后重启；最后一次成功写入生效 |
| `Tables` 为 nil 且记忆/RAG 已开启 | `Setup` 返回 `chat-agent: 需要 TableStorage 支持`，插件不装配 | 使用内置后端或自实现 TableStorage，或关闭两个功能 |
| `CreateTable` 失败 | `Start` 返回 `chat-agent: 存储表初始化失败: <err>` | 修复存储后端后重启 |
| `ListRows` 失败或超时 | `Start` 返回 `chat-agent: 存储表加载失败: <err>` | 修复存储后端后重启 |
| 单行 JSON/base64/维度损坏 | 该行跳过并 warn，其余行正常 | 重新写入同 ID 行 |
| 任一 `d:` 行非法 | 本轮跳过回收并 warn，分块保留 | 修复或删除该文档行后重启 |
| 配置无超级管理员 | 启动 warn；`/chat` 全部拒绝 | 核心管理员用 `/chat-admin admins add` |
| 删除最后一个超级管理员 | 固定拒绝文案，不修改 | 先添加另一个管理员再删除 |
| 记忆/知识库任务失败 | 固定失败消息 | 查看日志，修正配置或数据后重试 |
| 异步完成消息发送失败 | 命令已生效，无完成消息 | 查看日志与状态命令 |
| `admins reset` 后配置为空 | 无插件超级管理员 | 核心管理员重新添加 |
| 核心管理员未配置 | `/chat-admin` 被宿主拒绝 | 配置宿主 Auth |
| 向量索引未就绪 | RAG 跳过，聊天继续 | 等待加载完成或查看 `/chat vector status` |

### 14.8 示例

示例 1：查看状态

```text
用户：/chat status
chat-agent: enabled=true kind=group policy=open history=12 vector=ready memory=8 knowledge=2/14
```

示例 2：配置群聊白名单

```text
用户：/chat policy group whitelist
policy: group=whitelist
用户：/chat list group add telegram:-100123
list group: 已添加 telegram:-100123
用户：/chat list group
list group: telegram:-100123
```

示例 3：添加插件超级管理员

```text
用户：/chat admins add telegram:123456789
admins: 已添加 telegram:123456789
用户：/chat admins
admins: telegram:123456789
```

示例 4：搜索记忆

```text
用户：/chat memory search 部署方式
memory search: 2 条结果
3f9a1c2b7d4e5f60 0.842 fact 持久化需要 sqlite 或 mysql 后端
a1b2c3d4e5f60718 0.511 preference 用户偏好简短回答
```

示例 5：导入知识库

```text
用户：/chat knowledge add https://example.com/deploy 部署说明
knowledge: 已开始 https://example.com/deploy
knowledge: 完成 doc=7d4e5f603f9a1c2b chunks=14 result=added
```

示例 6：手动整理与清理

```text
用户：/chat jobs run consolidate
jobs: 已触发 consolidate
jobs: 完成 consolidate: sessions_done=3 sessions_failed=0 memories_added=5 memories_merged=2

用户：/chat jobs run cleanup
jobs: 已触发 cleanup
jobs: 完成 cleanup: sessions_removed=4 histories_trimmed=7
```

示例 7：向量状态与重建

```text
用户：/chat vector status
vector: state=ready model=text-embedding-3-small dims=1536 memory=8 knowledge=14 stale=0
用户：/chat vector reindex
vector: 已开始重建
vector: 完成 reindexed=0
```

示例 8：核心管理员执行

```text
用户：/chat-admin status
chat-agent: enabled=true kind=private policy=whitelist history=3 vector=ready memory=0 knowledge=0/0
用户：/chat-admin admins add telegram:123456789
admins: 已添加 telegram:123456789
```

## 相关文档

- 名单语义：[`participation.md §7.8`](participation.md#78-名单策略)。
- 记忆命令行为：[`memory.md §9.7`](memory.md#97-管理命令)。
- 知识库命令行为：[`rag.md §10.8`](rag.md#108-管理命令)。
- 表与行形状：[`architecture.md §4.4`](architecture.md#44-storage-模式)。
- 配置键：[`configuration.md §13.1`](configuration.md#131-权威配置键表)。
- 测试矩阵：[`testing.md §15.2`](testing.md#152-单元测试矩阵)。
