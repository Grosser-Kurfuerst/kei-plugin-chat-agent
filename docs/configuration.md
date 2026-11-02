# 配置

覆盖：全部配置键、默认值、校验、示例、环境变量、生效方式、`Metadata.Options` 与密钥处理。不覆盖：运行期命令语义（见 [`admin.md`](admin.md) 第 14 章）、算法中的预算使用（见 [`conversation.md`](conversation.md)、[`memory.md`](memory.md)、[`rag.md`](rag.md)）。

## 13. 配置

### 13.1 权威配置键表

本节是配置键的唯一权威。所有键都是 `plugins.chat-agent` 下的扁平 `snake_case` 键，不在本节表中的键不是配置键。`enabled` 是核心保留键，控制插件是否装配，不属于插件 `Settings`，因此不列入本节、示例键集合或 `Metadata.Options`。`Metadata.Options` 必须包含本节全部顶层键。

#### 13.1.1 名单与准入

| 键 | 类型 | 默认 | 校验/说明 |
|---|---|---|---|
| `group_policy` | string | `open` | `off\|open\|whitelist\|blacklist` |
| `group_list` | []string | `[]` | 条目非空；可写标量、逗号分隔或列表；去重且保持首次出现顺序 |
| `private_policy` | string | `off` | 同上四模式 |
| `private_list` | []string | `[]` | 同上 |
| `ignore_bots` | bool | `true` | 忽略 `Sender.IsBot` 的消息 |
| `respond_to_commands` | bool | `false` | 命令消息是否进入聊天；默认跳过且不进历史 |
| `self_ids` | []string | `[]` | 机器人自身用户 ID；用于 @ 判定 |
| `trigger_keywords` | []string | `[]` | 群聊关键词寻址；子串匹配；去重保序 |
| `trigger_min_chars` | int | `2` | `>= 0`；去首尾空白后的文本少于该 rune 数不触发 |
| `super_admins` | []string | `[]` | 每项必须是 `platform:userID`；两侧非空；去重保序 |

#### 13.1.2 参与与回复

| 键 | 类型 | 默认 | 校验/说明 |
|---|---|---|---|
| `mention_reply_probability` | float | `1.0` | `0..1`；被寻址/私聊时的回复概率 |
| `mention_min_interval` | duration | `10s` | `>= 0`；被寻址/私聊的冷却 |
| `random_enabled` | bool | `true` | 群聊随机插话总开关 |
| `random_probability` | float | `0.12` | `0..1` |
| `random_cooldown` | duration | `90s` | `>= 0` |
| `random_max_per_hour` | int | `6` | `>= 0`；0 表示随机插话关闭 |
| `random_min_participants` | int | `2` | `>= 0`；窗口内不同非机器人发言者数 |
| `random_activity_window` | duration | `5m` | `> 0` |
| `random_quiet_hours` | string | `""` | `HH:MM-HH:MM`，支持跨零点；`start==end` 表示禁用 |
| `random_timezone` | string | `Local` | `time.LoadLocation` 可解析；影响静默时段与 `{{now}}` |
| `batch_window` | duration | `2500ms` | `> 0` |
| `batch_max_window` | duration | `8s` | `>= batch_window` |
| `limits_max_concurrent` | int | `2` | `>= 1`；全局生成并发上限 |
| `reply_max_chars` | int | `200` | `>= 1`；按 rune 截断 |
| `reply_mention_sender` | bool | `false` | 群聊回复时是否 `At` 最后一条消息的发送者 |
| `reply_quote` | bool | `true` | 是否引用本批最后一条消息；无消息 ID 时省略 |
| `reply_dedupe` | bool | `true` | 与本会话最近 10 分钟内最近 5 条机器人回复去重 |

#### 13.1.3 历史与上下文

| 键 | 类型 | 默认 | 校验/说明 |
|---|---|---|---|
| `history_max_messages` | int | `30` | `>= 1`；每会话内存历史条数上限 |
| `history_max_chars` | int | `8000` | `>= 1`；每会话历史总 rune 上限 |
| `history_max_sessions` | int | `512` | `>= 1`；内存会话 LRU 上限 |
| `history_ttl` | duration | `24h` | `>= 0`；0 表示不按空闲时间过期，LRU 仍生效 |
| `history_unconsolidated_ttl` | duration | `72h` | `> 0`；未整理历史的硬上限 |
| `history_cleanup_interval` | duration | `5m` | `>= 0`；0 关闭定时清理 |
| `context_max_messages` | int | `20` | `>= 1`；拼进提示词的历史条数 |
| `context_max_chars` | int | `4000` | `>= 1`；拼进提示词的历史 rune 上限 |

#### 13.1.4 Chat 模型与提示词

| 键 | 类型 | 默认 | 校验/说明 |
|---|---|---|---|
| `bot_name` | string | `助手` | 非空；提示词与历史中的机器人显示名 |
| `system_prompt` | string | 见 [`conversation.md §8.3`](conversation.md#83-上下文渲染) | 空串使用默认模板；非空整体覆盖默认模板 |
| `llm_base_url` | string | `https://api.openai.com/v1` | 带 host 的 http(s) URL，无 userinfo |
| `llm_api_key` | string | `""` | 允许空（本地服务） |
| `llm_model` | string | `""` | 必填；非空 |
| `llm_temperature` | float | `0.8` | `>= 0` |
| `llm_max_tokens` | int | `400` | `>= 1` |
| `llm_timeout` | duration | `30s` | `> 0` |
| `llm_max_retries` | int | `1` | `>= 0` |
| `llm_skip_token` | string | `[SKIP]` | 非空 |
| `llm_skip_enabled` | bool | `true` | 随机轮次是否允许模型用 skip token 拒答 |
| `llm_extra_headers` | map[string]string | `{}` | 不得覆盖 `authorization`、`content-type`、`content-length`、`host` |
| `debug_prompts` | bool | `false` | 为 true 时以 Debug 记录完整请求/响应提示词 |
| `vision_enabled` | bool | `false` | 开启图片识别与图片记忆；需视觉 LLM；图片记忆仍受 `memory_enabled` 控制 |

#### 13.1.5 Embedding 与向量索引

| 键 | 类型 | 默认 | 校验/说明 |
|---|---|---|---|
| `memory_enabled` | bool | `true` | 长期记忆子系统总开关 |
| `rag_enabled` | bool | `true` | 知识库 RAG 子系统总开关 |
| `embedding_base_url` | string | `""` | 空串回落 `llm_base_url`；否则同 URL 校验 |
| `embedding_api_key` | string | `""` | 空串回落 `llm_api_key` |
| `embedding_model` | string | `""` | `memory_enabled` 或 `rag_enabled` 为 true 时必填 |
| `embedding_dimensions` | int | `0` | `>= 0`；0 表示由服务端决定；>0 时响应必须匹配 |
| `embedding_timeout` | duration | `20s` | `> 0` |
| `embedding_max_retries` | int | `1` | `>= 0` |
| `embedding_extra_headers` | map[string]string | `{}` | 与 `llm_extra_headers` 同一禁改列表 |
| `vector_max_records` | int | `10000` | `>= 1`；`vector` 表中可见 memory + 被引用 knowledge chunk 行数上限（含 stale；不含 docs/meta/未引用 chunk） |

#### 13.1.6 长期记忆

| 键 | 类型 | 默认 | 校验/说明 |
|---|---|---|---|
| `memory_consolidate_interval` | duration | `30m` | `>= 0`；0 关闭定时整理 |
| `memory_consolidate_max_sessions` | int | `16` | `>= 1`；每轮最多处理的会话数 |
| `memory_consolidate_max_messages` | int | `50` | `>= 1`；每会话每轮最多读取的未整理消息数 |
| `memory_consolidate_timeout` | duration | `5m` | `> 0`；单轮任务总超时 |
| `memory_extract_max` | int | `8` | `>= 0`；单次抽取最多条数；0 表示不抽取 |
| `memory_extract_min_messages` | int | `2` | `>= 1`；少于此数不调用 LLM |
| `memory_dedupe_threshold` | float | `0.92` | `0..1`；余弦相似度达到即合并 |
| `memory_min_score` | float | `0.35` | `0..1`；检索最低分 |
| `memory_top_k` | int | `5` | `>= 0`；0 关闭记忆检索 |
| `memory_context_max_chars` | int | `1200` | `>= 0`；记忆注入 rune 上限 |
| `memory_importance_decay_half_life` | duration | `720h` | `> 0`；默认 30 天 |
| `memory_min_importance` | float | `0.15` | `0..1`；衰减后低于此值删除 |
| `memory_ttl` | duration | `0` | `>= 0`；0 表示不按 `last_used_at` 过期 |

#### 13.1.7 知识库 RAG

| 键 | 类型 | 默认 | 校验/说明 |
|---|---|---|---|
| `rag_context_max_chars` | int | `2400` | `>= 0`；记忆 + 知识库注入总上限 |
| `knowledge_top_k` | int | `3` | `>= 0`；0 关闭知识库检索 |
| `knowledge_min_score` | float | `0.30` | `0..1` |
| `knowledge_context_max_chars` | int | `1200` | `>= 0`；知识库注入 rune 上限 |
| `knowledge_chunk_chars` | int | `800` | `>= 100` |
| `knowledge_chunk_overlap` | int | `100` | `>= 0` 且 `< knowledge_chunk_chars` |
| `knowledge_max_bytes` | int | `1048576` | `>= 1`；抓取/正文上限 |
| `knowledge_allowed_content_types` | []string | `["text/plain","text/markdown","text/html"]` | 非空；小写、去参数后匹配 |
| `knowledge_url_allow_private` | bool | `false` | 允许抓取回环/私网/保留地址；仅测试用 |

#### 13.1.8 任务

| 键 | 类型 | 默认 | 校验/说明 |
|---|---|---|---|
| `jobs_run_on_start` | bool | `false` | 启动后立即各跑一次整理与清理 |

未列入本表的键不是配置键。内部常量不暴露配置：`storage_timeout=2s`、`table_name="vector"`、`table_meta_key="meta"`、`table_load_timeout=30s`、`shutdown_wait_timeout=5s`、`vector_load_workers=4`、`vector_ready_timeout=2s`、`context_message_max_chars=1000`、`embedding_batch_size=16`、`embedding_max_input_chars=6000`、`knowledge_fetch_timeout=20s`、`knowledge_max_redirects=3`、`knowledge_max_chunks_per_doc=500`、`knowledge_user_agent="kei-plugin-chat-agent/0.1"`、`memory_extract_temperature=0.2`、`memory_extract_max_tokens=800`、`history_cleanup_timeout=1m`、`vision_fetch_timeout=10s`、`vision_max_images_per_request=4`、`vision_max_image_bytes=4194304`、`vision_max_redirects=3`、`vision_allowed_formats=image/jpeg,image/png,image/gif,image/webp`（白名单函数，不是可变全局）。

### 13.2 示例配置

宿主启用方式：设置 `plugins.chat-agent.enabled: true`。`enabled` 是核心保留键，不属于插件键表。

插件键（位于 `plugins.chat-agent` 下，与 `enabled` 平铺）：

```yaml
group_policy: open
private_policy: whitelist
private_list:
  - "telegram:123456789"
ignore_bots: true
respond_to_commands: false
self_ids:
  - "telegram:987654321"
trigger_keywords:
  - "小助手"
  - "assistant"
trigger_min_chars: 2
super_admins:
  - "telegram:123456789"

mention_reply_probability: 1.0
mention_min_interval: 10s
random_enabled: true
random_probability: 0.12
random_cooldown: 90s
random_max_per_hour: 6
random_min_participants: 2
random_activity_window: 5m
random_quiet_hours: "23:00-07:00"
random_timezone: "Asia/Shanghai"
batch_window: 2500ms
batch_max_window: 8s
limits_max_concurrent: 2
reply_max_chars: 200
reply_mention_sender: false
reply_quote: true
reply_dedupe: true

history_max_messages: 30
history_max_chars: 8000
history_max_sessions: 512
history_ttl: 24h
history_unconsolidated_ttl: 72h
history_cleanup_interval: 5m
context_max_messages: 20
context_max_chars: 4000

bot_name: "助手"
system_prompt: ""
llm_base_url: "https://api.openai.com/v1"
llm_api_key: ""
llm_model: "gpt-4o-mini"
llm_temperature: 0.8
llm_max_tokens: 400
llm_timeout: 30s
llm_max_retries: 1
llm_skip_token: "[SKIP]"
llm_skip_enabled: true
llm_extra_headers: {}
debug_prompts: false
vision_enabled: false

memory_enabled: true
rag_enabled: true
embedding_base_url: ""
embedding_api_key: ""
embedding_model: "text-embedding-3-small"
embedding_dimensions: 1536
embedding_timeout: 20s
embedding_max_retries: 1
embedding_extra_headers: {}
vector_max_records: 10000

memory_consolidate_interval: 30m
memory_consolidate_max_sessions: 16
memory_consolidate_max_messages: 50
memory_consolidate_timeout: 5m
memory_extract_max: 8
memory_extract_min_messages: 2
memory_dedupe_threshold: 0.92
memory_min_score: 0.35
memory_top_k: 5
memory_context_max_chars: 1200
memory_importance_decay_half_life: 720h
memory_min_importance: 0.15
memory_ttl: 0

rag_context_max_chars: 2400
knowledge_top_k: 3
knowledge_min_score: 0.30
knowledge_context_max_chars: 1200
knowledge_chunk_chars: 800
knowledge_chunk_overlap: 100
knowledge_max_bytes: 1048576
knowledge_allowed_content_types:
  - "text/plain"
  - "text/markdown"
  - "text/html"
knowledge_url_allow_private: false

jobs_run_on_start: false
```

示例中 `llm_api_key` 为空仅适用于本地服务；生产环境用环境变量提供。示例没有列出的键使用 §13.1 的默认值。

### 13.3 环境变量

环境变量由宿主在装配插件前覆盖同名配置键：

```text
KEI_PLUGINS_CHAT_AGENT_<KEY>
```

规则：

- 键名转大写，`-` 与 `.` 转 `_`；插件键本身是扁平 `snake_case`，因此映射是直接的。
- 只匹配已存在的插件；不会因为环境变量而创建插件。
- 空值视为未设置，不覆盖配置。
- 值按 YAML 标量推断类型：`true`/`false` → bool，整数 → int，`0.5` → float，其余 → string；列表可用逗号分隔或 YAML 列表语法。
- 密钥类变量在宿主日志中掩码；插件不记录其值。

示例：

```bash
export KEI_PLUGINS_CHAT_AGENT_LLM_API_KEY="sk-..."
export KEI_PLUGINS_CHAT_AGENT_LLM_MODEL="gpt-4o-mini"
export KEI_PLUGINS_CHAT_AGENT_GROUP_POLICY="whitelist"
export KEI_PLUGINS_CHAT_AGENT_GROUP_LIST="telegram:-100123,telegram:-100456"
export KEI_PLUGINS_CHAT_AGENT_MEMORY_ENABLED="false"
export KEI_PLUGINS_CHAT_AGENT_VISION_ENABLED="true"
export KEI_PLUGINS_CHAT_AGENT_RANDOM_QUIET_HOURS="23:00-07:00"
```

### 13.4 校验规则

统一错误格式：

```text
chat-agent: 配置错误 <key>=<值>: <原因>
```

原因字面量只允许以下集合，`<n>` 替换为对应下限：

```text
不能为空
条目不能为空
必须是布尔值
必须是整数
必须是数字
必须是时长
必须是字符串
条目必须是 platform:id
必须是 off|open|whitelist|blacklist 之一
必须是 0..1 之间的小数
必须 >= <n>
必须 > 0
必须是 HH:MM-HH:MM 格式
不是合法时区
必须是 platform:userID
必须是带 host 的 http(s) URL
不得包含 userinfo
必须是字符串键值映射
必须是非空对象
不得覆盖协议请求头
llm_model 不能为空
memory_enabled 或 rag_enabled 为 true 时必须配置 embedding_model
batch_max_window 必须 >= batch_window
knowledge_chunk_overlap 必须 >= 0 且 < knowledge_chunk_chars
knowledge_allowed_content_types 不能为空
```

分组：

| 键组 | 校验 |
|---|---|
| 字符串非空 | `bot_name`、`llm_skip_token`、`llm_model`（`llm_model 不能为空`） |
| 四模式枚举 | `group_policy`、`private_policy` |
| 0..1 浮点 | `mention_reply_probability`、`random_probability`、`memory_dedupe_threshold`、`memory_min_score`、`memory_min_importance`、`knowledge_min_score` |
| `>= 0` | `trigger_min_chars`、`mention_min_interval`、`random_cooldown`、`random_max_per_hour`、`random_min_participants`、`history_ttl`、`history_cleanup_interval`、`llm_temperature`、`llm_max_retries`、`embedding_dimensions`、`embedding_max_retries`、`memory_consolidate_interval`、`memory_extract_max`、`memory_top_k`、`memory_context_max_chars`、`memory_ttl`、`rag_context_max_chars`、`knowledge_top_k`、`knowledge_context_max_chars`、`knowledge_chunk_overlap` |
| `>= 1` | `limits_max_concurrent`、`reply_max_chars`、`history_max_messages`、`history_max_chars`、`history_max_sessions`、`context_max_messages`、`context_max_chars`、`llm_max_tokens`、`vector_max_records`、`memory_consolidate_max_sessions`、`memory_consolidate_max_messages`、`memory_extract_min_messages`、`knowledge_max_bytes` |
| `> 0` | `random_activity_window`、`batch_window`、`llm_timeout`、`embedding_timeout`、`memory_consolidate_timeout`、`memory_importance_decay_half_life`、`history_unconsolidated_ttl` |
| 静默时段 | `random_quiet_hours` |
| 时区 | `random_timezone` |
| 管理员 | `super_admins` |
| URL | `llm_base_url`、`embedding_base_url` |
| 请求头映射 | `llm_extra_headers`、`embedding_extra_headers` |
| 条件必填 | `embedding_model` |
| 批处理窗口 | `batch_max_window` |
| 分块 | `knowledge_chunk_chars`（`>= 100`）、`knowledge_chunk_overlap` |
| 内容类型 | `knowledge_allowed_content_types` |
| 列表条目 | `group_list`、`private_list`、`self_ids`、`trigger_keywords`、`super_admins`、`knowledge_allowed_content_types` |

补充：

- 列表接受标量、逗号分隔字符串或 YAML 列表；条目去首尾空白，空条目报 `条目不能为空`。`group_list`/`private_list` 的 `platform:id` 条目两侧必须非空，否则报 `条目必须是 platform:id`；平台条目在加载时统一 trim。
- 标量类型必须可解析：布尔键接受 bool 或可解析的字符串，整数/浮点/时长键接受对应数字或可解析字符串；无法解析返回固定的类型错误，不再静默回落默认值。
- 所有字符串配置值在加载时 trim 首尾空白；`system_prompt` 也按此规则处理。
- `super_admins` 每项必须是 `platform:userID`，两侧非空；平台与用户 ID 两侧空白会被规范化。
- `random_quiet_hours` 为空串合法；非空必须是 `HH:MM-HH:MM`，`start == end` 合法但禁用。
- `llm_api_key` 与 `embedding_api_key` 允许空串。
- `system_prompt` 允许空串；空串使用默认模板。
- `embedding_base_url` 为空串合法；非空时按 URL 校验。
- 布尔键接受 bool 或 `true`/`false` 字符串；无法解析报 `必须是布尔值`。`vision_enabled` 不探测模型能力；为 true 时 `llm_model` 必须接受 OpenAI 多模态 `content` 数组（`text` 与 `image_url` 块）；`memory_enabled=true` 时图片识别文本由 `embedding_model` 嵌入，Embeddings 请求仍只包含文本。
- `vision_enabled=true` 且 `memory_enabled=false` 合法：图片可被识别，但不写长期记忆。`vision_enabled=false` 时不下载图片，纯图片消息按 [`participation.md §7.1`](participation.md#71-事件过滤与准入) 拒绝。
- 任一键校验失败即返回错误，插件不装配；错误信息包含规范化前的原值。
- 未列入 §13.1 的键不读取、不校验。

### 13.5 配置来源与生效

- 配置只从 `PluginContext.Config` 读取；插件不直接读环境变量或文件。
- 宿主启动时合并环境变量覆盖，再交给插件；插件只看到最终值。
- 配置不热更新；修改后必须重启宿主。
- 运行期只能通过命令覆盖：`/chat policy`、`/chat list`、`/chat admins`、`/chat on|off|reset`。
- 其它键（模型、Embedding、历史上限、任务间隔）修改后需要重启。
- 配置校验失败时插件不装配，核心记录错误；不会以部分默认值启动。

### 13.6 Metadata.Options

`Metadata.Options` 必须包含 §13.1 的全部 82 个顶层键，与 [`architecture.md §6`](architecture.md#6-与-kei-核心的契约对应) 的 `Options` 代码块逐项一致。核心在漏列时记录 warn，但不拒绝装配；插件仍按 §13.1 读取。任何键增删必须同时修改五处：§13.1、本节清单、[`architecture.md §6`](architecture.md#6-与-kei-核心的契约对应) 的 `Metadata.Options`、§13.2 示例配置、§13.4 校验规则。

以下清单是 `Metadata.Options` 的镜像，共 82 项：

```text
group_policy
group_list
private_policy
private_list
ignore_bots
respond_to_commands
self_ids
trigger_keywords
trigger_min_chars
super_admins
mention_reply_probability
mention_min_interval
random_enabled
random_probability
random_cooldown
random_max_per_hour
random_min_participants
random_activity_window
random_quiet_hours
random_timezone
batch_window
batch_max_window
limits_max_concurrent
reply_max_chars
reply_mention_sender
reply_quote
reply_dedupe
history_max_messages
history_max_chars
history_max_sessions
history_ttl
history_unconsolidated_ttl
history_cleanup_interval
context_max_messages
context_max_chars
bot_name
system_prompt
llm_base_url
llm_api_key
llm_model
llm_temperature
llm_max_tokens
llm_timeout
llm_max_retries
llm_skip_token
llm_skip_enabled
llm_extra_headers
debug_prompts
vision_enabled
memory_enabled
rag_enabled
embedding_base_url
embedding_api_key
embedding_model
embedding_dimensions
embedding_timeout
embedding_max_retries
embedding_extra_headers
vector_max_records
memory_consolidate_interval
memory_consolidate_max_sessions
memory_consolidate_max_messages
memory_consolidate_timeout
memory_extract_max
memory_extract_min_messages
memory_dedupe_threshold
memory_min_score
memory_top_k
memory_context_max_chars
memory_importance_decay_half_life
memory_min_importance
memory_ttl
rag_context_max_chars
knowledge_top_k
knowledge_min_score
knowledge_context_max_chars
knowledge_chunk_chars
knowledge_chunk_overlap
knowledge_max_bytes
knowledge_allowed_content_types
knowledge_url_allow_private
jobs_run_on_start
```

### 13.7 密钥与安全

- API key 只通过 `llm_api_key`/`embedding_api_key` 或对应环境变量提供；不写入代码、文档示例或提交记录。
- 推荐环境变量：`KEI_PLUGINS_CHAT_AGENT_LLM_API_KEY`、`KEI_PLUGINS_CHAT_AGENT_EMBEDDING_API_KEY`。
- 插件日志不记录 key、Authorization 头或请求体；宿主负责环境变量的掩码。
- `debug_prompts=true` 会记录完整提示词与响应，其中可能包含用户消息、记忆与知识库正文；生产环境默认关闭。
- `llm_extra_headers`/`embedding_extra_headers` 不得覆盖协议头；校验失败阻止启动。
- URL 不得包含 userinfo，避免把凭据写进日志或错误消息。
- `knowledge_url_allow_private=true` 只允许在隔离测试环境使用，生产必须保持 `false`。
- 图片 URL 来自消息，按不可信输入处理：只允许 http(s)、禁止回环/私网/保留地址、限制重定向、单图大小与格式；图片客户端从宿主 transport 派生并禁用代理，避免代理侧解析绕过地址校验；`KeyFile` 不支持，插件不读本地文件。原始 URL 与 data URL 不写日志；`debug_prompts=true` 时图片折叠为 `[image <mime> <bytes>B]` 摘要。

## 相关文档

- 配置键使用：[`conversation.md §8.3`](conversation.md#83-上下文渲染)、[`memory.md`](memory.md) 第 9 章、[`rag.md`](rag.md) 第 10 章。
- `Metadata.Options` 镜像：[`architecture.md §6`](architecture.md#6-与-kei-核心的契约对应)。
- 命令覆盖：[`admin.md §14.4`](admin.md#144-持久化与优先级)。
- 校验测试：[`testing.md §15.2`](testing.md#152-单元测试矩阵)。
