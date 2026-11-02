# 测试与质量

覆盖：质量门、单元测试矩阵、测试注入缝、mock 适配器端到端场景、竞态与优雅关闭、文档校验与完成定义。不覆盖：实现步骤（见 [`roadmap.md`](roadmap.md) 第 16 章）、具体算法定义（见第 7-12 章）。

## 15. 测试与质量

### 15.1 质量门

质量门适用于所有代码变更。任何代码变更在提交前必须通过：

```bash
gofmt -l .
go build ./...
go vet ./...
go test ./...
go test -race ./...
```

执行环境：

- flake devShell 提供 Go 与 C 工具链；Go 具体版本以 `flake.lock` 固定的 nixpkgs 为准。
- `GOTOOLCHAIN=local`，禁止自动下载工具链。
- SQLite 后端测试使用 `CGO_ENABLED=1`；其它包不要求 CGO。
- `gofmt -l .` 必须无输出。
- 测试命令在仓库根执行；不依赖网络，LLM/Embedding 全部用本地桩。

实现阶段每一阶段都必须满足“可编译、插件可启动、该阶段单测通过、`go test -race` 通过”；P8 另加端到端与文档校验。

### 15.2 单元测试矩阵

#### 15.2.1 配置

| 用例 | 可观察断言 |
|---|---|
| 全空配置读取默认值 | 每个键等于 [`configuration.md §13.1`](configuration.md#131-权威配置键表) 默认值 |
| 四模式枚举逐值 | 合法值通过；其它值返回 `必须是 off\|open\|whitelist\|blacklist 之一` |
| 0..1 浮点边界 | 0、1 通过；-0.1、1.1 返回 `必须是 0..1 之间的小数` |
| 整数下界 | 等于下限通过；小于下限返回 `必须 >= <n>` |
| 时长下界 | 0 与正数按规则；负数与零按 `>= 0`/`> 0` 返回 |
| URL 校验 | 缺 host、非 http(s)、带 userinfo 分别返回对应原因 |
| 时区 | `Local`、`UTC`、`Asia/Shanghai` 通过；非法值返回 `不是合法时区` |
| 静默时段 | 空串、`00:00-00:00`、跨零点通过；`25:00-01:00` 返回格式错误 |
| 列表解析 | 标量、逗号分隔、YAML 列表得到相同顺序；重复去重；空条目报错 |
| 管理员条目 | 合法 `platform:userID` 通过；缺冒号、空侧返回 `必须是 platform:userID` |
| headers 映射 | 普通键通过；覆盖协议头返回 `不得覆盖协议请求头` |
| 条件必填 | 两开关都关时 `embedding_model` 可为空；任一开时为空报错 |
| 图片开关 | `vision_enabled` 默认 false；true/false 均可读取；开启不要求 `memory_enabled` |
| 批窗口关系 | `batch_max_window < batch_window` 返回固定原因 |
| 分块关系 | overlap 为负或 `>= chunk_chars` 返回固定原因 |
| 内容类型 | 空列表报错；大小写与参数被规范化 |
| 错误格式 | 首个错误包含键名、原值与原因 |
| `Metadata.Options` | 集合与 §13.1 的 82 个键完全一致 |

#### 15.2.2 名单

| 用例 | 可观察断言 |
|---|---|
| `off` 群聊/私聊 | 全部返回 `not_allowed`，不建会话、不进历史 |
| `open` 群聊/私聊 | 通过准入，进入后续判定 |
| `whitelist` 命中/未命中 | 命中通过；未命中 `not_allowed` |
| `blacklist` 命中/未命中 | 命中拒绝；未命中通过 |
| 空名单 | `open`/`blacklist` 放行；`whitelist` 全拒；`off` 全拒 |
| 限定条目 | `telegram:id` 只匹配该平台；裸 ID 跨平台匹配 |
| 拒绝无副作用 | 会话 map、历史、活跃时间、参与人数均不变化 |
| 管理命令绕过 | 关闭会话与 `off` 模式下 `/chat` 仍可执行 |
| 覆盖加载 | `policy` JSON 覆盖配置；`[]` 覆盖为空 |
| 非法覆盖 | 模式非法或 JSON 损坏时回落配置并 warn |
| `reset` | 删除键后恢复配置值 |

#### 15.2.3 决策

| 用例 | 可观察断言 |
|---|---|
| reason 全覆盖 | [`participation.md §7.7`](participation.md#77-决策日志与-reason-词表) 的 21 个值逐一构造事件，日志 reason 精确匹配 |
| @：`self_ids` 非空 | 匹配、非匹配、`all` 三态分别寻址、不寻址、不寻址 |
| @：`self_ids` 为空 | 任意非 `all` 的 `SegAt` 寻址；`all` 不寻址 |
| 引用寻址 | 命中最近 128 条机器人 ID 环；不在环内不寻址 |
| 关键词 | ASCII 大小写不敏感；中文子串；空列表不寻址 |
| 私聊 | 恒寻址，不受 @/引用/关键词影响 |
| 引用段 | `reply_quote` 使用 `Message.ID`；事件 ID 与消息 ID 不同也不串用 |
| 概率 0/1 | 0 永不回复；1 在其它条件满足时必回复 |
| 冷却 | `mention_min_interval` 内返回 `cooldown`，之后恢复 |
| 随机开关 | `random_enabled=false` 返回 `not_addressed` |
| 小时配额 | `random_max_per_hour=0` 与达到上限都返回 `hour_quota` |
| 静默跨零点 | 23:00-07:00 在 23:30 与 06:59 命中，07:00 不命中 |
| 参与人数 | 少于阈值返回 `min_participants`；达到阈值通过 |
| 批处理合并 | 窗口内多条合并为一批，按 `seq` 升序 |
| 最大窗口 | 后续消息不能把触发时间推过 `firstAt+batch_max_window` |
| 在途 pending | 生成期间消息进入 pending，完成后 100ms 触发下一批 |
| epoch | 删除后旧 timer 触发被丢弃，reason `stale` |
| 信号量满 | 丢弃整批并记 `semaphore_full`，不排队 |
| 过短/命令/机器人/自身 | 分别返回 `too_short`、`command`、`bot_sender`、`self_sender` |
| 纯非文本（vision 关闭） | 返回 `empty_text`，不记录 |
| 纯图片（vision 开启） | 进入历史，`Text=[图片]`，`Images` 保留 URL |
| 图片与 `trigger_min_chars` | 含可用图片的消息跳过文本长度检查 |
| `KeyFile` 图片 | 不进入 `Images`；纯 `KeyFile` 消息返回 `empty_text` |

#### 15.2.4 历史

| 用例 | 可观察断言 |
|---|---|
| 条数裁剪 | 超过 `history_max_messages` 时保留最新，`lostSeq` 推进 |
| 字符裁剪 | 总 rune 超限时从最旧裁剪，`lostSeq` 只覆盖未整理部分 |
| 序号 | 首条 `Seq=0`，每次追加递增，机器人回复也递增 |
| 占位符 | 图片/表情/文件/卡片/引用/未知段映射为固定占位符 |
| 图片字段 | vision 开启时 `Images` 按出现顺序保存 URL；关闭时为空 |
| 图片预算 | 图片 URL 不计入 `history_max_chars` / `context_max_chars` |
| TTL | 空闲超过 `history_ttl` 且已整理的会话被删除 |
| `history_ttl=0` | 不按空闲删除，LRU 与硬上限仍生效 |
| 未整理保护 | 未整理会话不被普通 TTL 删除 |
| 硬 TTL | 超过 `history_unconsolidated_ttl` 时删除并 warn |
| LRU | 超过 `history_max_sessions` 时优先淘汰已整理会话 |
| 在途跳过 | inflight/pending/timer 的会话本轮不删 |
| 删除清理 | timer 停止、pending 清空、epoch 递增、覆盖保留 |
| 重启 | 历史从空开始，覆盖从 Storage 懒加载 |

#### 15.2.5 提示词

| 用例 | 可观察断言 |
|---|---|
| 默认模板 | 空 `system_prompt` 渲染出 [`conversation.md §8.3`](conversation.md#83-上下文渲染) 的三行模板 |
| 覆盖模板 | 非空 `system_prompt` 完全替换默认模板 |
| 五个占位符 | `bot_name`、`chat_kind`、`now`、`user_name`、`channel_name` 正确替换 |
| 未知占位符 | 原样保留，不二次展开 |
| 历史块 | 群聊/私聊头正确；只包含 `Seq < firstSeq` 的条目 |
| 本轮块 | 固定 `[本轮消息]`，每条 `显示名: 文本` |
| 预算 | 条数与字符上限生效；单条截断 1000 rune |
| 记忆块 | 有结果时格式逐字匹配；无结果整块省略 |
| 知识库块 | 有结果时格式逐字匹配；无结果整块省略 |
| 视觉关闭 | `user.content` 为字符串，图片只保留 `[图片]` |
| 视觉开启 | `user.content` 为 parts；`image_url` 为 data URL，顺序与占位符一致 |
| 图片上限 | 单次最多 4 张；下载失败不占名额，继续尝试更旧图片 |
| 图片失败 | 保留占位符，文本部分正常生成 |
| 清洗 | `<`/`>` 转 `‹`/`›`，控制字符转空格 |
| skip token | 独立出现时被移除；清空后不发送 |
| 回复清洗 | CRLF 归一、3 个以上换行压成 2 个、去首尾空白 |
| 去重 | 最近 5 条、10 分钟内的相同文本不发送 |
| 截断 | 超过 `reply_max_chars` 时按 rune 截断并保留 `…` |

#### 15.2.6 LLM 与 Embedding

| 用例 | 可观察断言 |
|---|---|
| Chat 200 | 解析 `choices[0].message.content` |
| Chat 429/500 | 按 1s、2s、4s 重试，最多 `1+max_retries` 次 |
| Chat 400 | 不重试，返回 `http_4xx` |
| Chat 非法 JSON | 返回 `invalid_json`，不 panic |
| Chat 缺 choices | 返回 `invalid_shape` |
| Chat content 非字符串 | 数组/对象/null 均返回 `invalid_shape` |
| Chat 空内容 | 返回错误，不进入发送 |
| Chat 视觉请求 | `image_url.url` 为 data URL；不包含原始图片 URL；最多 4 张 |
| Chat 视觉模型不支持 | 含图请求返回 4xx 时映射 `llm_error`，不回退纯文本 |
| Chat 多模态响应 | `content` 为数组/对象时返回 `invalid_shape` |
| Chat 超时/取消 | 超时按配置重试；取消立即返回 |
| skip token | 随机轮次识别；寻址轮次不识别 |
| Embedding 正常 | 按 `index` 排序后顺序与输入一致 |
| Embedding 数量不符 | 返回 `invalid_shape` |
| Embedding 顺序重复/缺失 | 返回 `invalid_shape` |
| Embedding 维度不符 | 返回 `invalid_shape` |
| Embedding NaN/Inf | 返回 `invalid_vector` |
| Embedding 零向量 | 返回 `invalid_vector` |
| Embedding 批量 | 16 条一批；空输入不发请求 |
| Embedding 截断 | 单条超过 6000 rune 被截断 |
| Embedding 图片 | 请求只含文本；图片识别文本正常嵌入，请求不含 `image_url` |
| headers 禁改 | 覆盖协议头在配置校验期失败 |

#### 15.2.7 向量索引

| 用例 | 可观察断言 |
|---|---|
| `CreateTable` 新建 | 表不存在时创建成功，`state` 进入 `loading` |
| `CreateTable` 已存在 | `bot.ErrTableExists` 视为成功，`Start` 不返回错误 |
| `ListRows` 正常 | 全部行解析完成，`state=ready`，计数正确 |
| `ListRows` 缺表 | `bot.ErrTableNotFound` 按加载失败处理 |
| 表行解析 | `m:`/`d:`/`c:`/`meta` 各自构造正确记录与计数 |
| 坏 `m:`/`d:`/`c:`/`meta` 行 | 单行 JSON/base64/维度损坏时跳过并 warn，其余行继续 |
| 未知前缀 | 行被跳过并 warn，不删除 |
| doc 提交前失败 | 旧 `d:` 行仍指向旧块，旧文档可检索；新块未引用 |
| doc 提交后崩溃 | 新文档可见，旧块未引用，启动回收删除 |
| 旧块删除失败 | 只 warn，旧块保持未引用，仍发完成消息 |
| 启动回收 | 只删除未被任何 `d:` 引用的 `c:` 行，被引用行保留 |
| 非法 doc 不回收 | 任一 `d:` 行非法时本轮不删除任何 `c:` 行并 warn |
| `meta` 缺失 | 按配置与已有行推导并尽力写回；`vector status` 报配置值、维度 0 |
| `meta` 非法 | 同缺失处理，`version != 1` 也按缺失处理 |
| top-k | 分数降序，同分 ID 升序 |
| 阈值 | 低于 `memory_min_score`/`knowledge_min_score` 被过滤 |
| 维度不匹配 | 查询维度与行 `dims` 不同的行跳过并 warn，不参与点积 |
| stale | 模型或维度不符的行不参与检索，计入 stale |
| reindex | 只处理 stale；全部 current 时 `reindexed=0` 且不写 `meta` |
| reindex 写 meta | 全部成功后 `meta` 为当前 `model`/`dims`；部分失败保留旧 `meta` |
| 容量 visible | 只计 `m:` 行与被引用 `c:` 行；`d:`/`meta`/未引用块不计数 |
| 容量 projected | 更新文档时 `visible - 旧块数 + 新块数` 超限返回 `vector_full`，不写新块 |
| 加载容量 | 启动后 `visible > vector_max_records` 时淘汰记忆至上限；知识块超限只记录 `capacity_exceeded` |
| 表物理临时超限 | 更新期间行数可超过上限，回收后回落 |
| 两功能关闭 | `state=disabled`，不建表、不加载 |
| 加载等待 | 2s 未就绪时 RAG 跳过，`rag_status=loading` |
| loading 命令 | 索引类命令输出 `chat-agent: 向量索引未就绪`；`vector status` 报已解析部分 |

#### 15.2.8 记忆

| 用例 | 可观察断言 |
|---|---|
| 抽取合法 JSON | 全部合法条目写入，水位推进 |
| 抽取空数组 | 成功，水位推进 |
| 抽取部分非法 | 合法条目写入，非法丢弃，水位推进 |
| 抽取全部非法 | 失败，水位不推进 |
| 抽取非 JSON | 失败，水位不推进 |
| 消息不足 | 不调用 LLM，不推进水位 |
| `memory_extract_max=0` | 不调用 LLM，水位推进 |
| 嵌入失败 | 不写记录，水位不推进 |
| 合并 | 相似度达到阈值时同 ID 合并，字段规则正确 |
| 新插入 | ID 确定性，sources 最多 8，计数从 0 开始 |
| 衰减 | 半衰期后 effective 减半；低于阈值删除 |
| TTL | `last_used_at` 超过 `memory_ttl` 删除 |
| 容量 | 满时先淘汰 stale 记忆，再淘汰最低 effective；知识块占满时不先淘汰记忆，直接 `vector_full` |
| scope | 群聊检索频道 + 发送者；私聊只检索发送者 |
| 强化 | 命中后 use_count/last_used_at/importance 在内存更新 |
| 强化落库 | 下一轮整理写入批量更新；版本冲突时恢复较新行；失败保留内存值 |
| 记忆命令 | status/add/del/reset 的输出与副作用；reset 删除损坏的未加载行 |
| 记忆搜索 | 先回 `memory search: 已开始`，后台完成发送结果；`memory_top_k=0` 不发起 embedding |
| 未启用 | 所有记忆命令输出未启用文案 |
| 图片抽取 | vision 开启时附加 `image_url`；识别文本写入普通 `m:` 行 |
| 图片分批 | 每批最多 4 张；全部批次成功后水位推进 |
| 图片批失败 | 任一批 LLM/Embedding/写行失败时水位不推进，重试幂等 |
| 图片下载失败 | 保留占位符，批次不失败，水位可推进 |
| 图片全部失败 | 按空抽取成功处理，水位推进 |
| vision 关闭 | 抽取不附加图片、不下载图片 |
| reindex 图片记忆 | 用 `text` 重嵌入，不重新下载图片 |

#### 15.2.9 知识库

| 用例 | 可观察断言 |
|---|---|
| URL scheme | 非 http(s) 返回 `url_rejected` |
| userinfo | 带 userinfo 返回 `url_rejected` |
| 私网地址 | 回环、10/8、127/8、169.254/16 等全部拒绝；主机名解析到私网也拒绝 |
| 重定向 | 每跳重新校验；超过 3 跳失败 |
| 拨号固定 | 受限 transport 在拨号时校验解析 IP 并禁用代理 |
| Content-Type | 缺失/空/不允许返回 `unsupported_type`；带参数可匹配 |
| 大小限制 | 超过 `knowledge_max_bytes` 返回 `too_large` |
| HTML 提取 | comment/script/style/noscript 去除，实体解码 |
| 标题 | 参数 > `<title>` > URL 段 > 正文前 40 rune |
| 分块 | 目标长度、边界优先、overlap、至少前进 1 rune |
| 去重 | 同文档相同文本只保留一份 |
| 块上限 | 超过 500 返回 `chunk_limit`，不写入 |
| unchanged | 同 ID 同哈希不重新嵌入 |
| updated | 内容变化时先写新 `c:` 行，再写 `d:` 行提交，最后删旧 `c:` 行 |
| title-only | 标题变化不重新嵌入，只更新 `d:` 行，`unchanged` |
| del | 先删 `d:` 行，再删旧 `c:` 行 |
| 提交前失败 | 旧 `d:` 行仍指向旧块，旧文档可检索 |
| 提交后崩溃 | 新 `d:` 行可见，旧块未引用，启动回收删除 |
| 非法 doc | 任一 `d:` 行非法时本轮不回收 `c:` 行并 warn |
| refresh | URL 重新抓取；text 文档 `unchanged` + 日志 skip |
| busy | 任务运行中返回 `knowledge: 失败 busy` |
| 本地文件 | `/chat knowledge add /tmp/x` 返回 `url_rejected` |

#### 15.2.10 管理员与调度

| 用例 | 可观察断言 |
|---|---|
| 限定 ID | `platform:userID` 精确匹配；大小写不同不匹配 |
| 非超管 | `/chat` 输出 `chat-agent: 无权执行该命令` |
| 核心管理员 | `/chat-admin status` 通过；`/chat` 仍按插件名单 |
| 最后管理员 | 删除唯一管理员被拒绝，集合不变 |
| reset | 删除覆盖，恢复配置；配置为空时 warn |
| 持久化 | 重启后 `policy`/`admins`/`session` 覆盖与 `vector` 表行保留 |
| `Tables` nil | 记忆或 RAG 开启时 `Setup` 返回 `chat-agent: 需要 TableStorage 支持`；两功能关闭时可启动 |
| 表初始化失败 | `Start` 返回 `chat-agent: 存储表初始化失败: <err>` |
| 表加载失败 | `Start` 返回 `chat-agent: 存储表加载失败: <err>` |
| 输出字面量 | 每条固定输出与 [`admin.md §14.3`](admin.md#143-子命令与输出) 逐字一致 |
| 异步完成 | 添加/删除/知识库/任务/重建均发送完成或失败消息 |
| interval=0 | ticker 不触发；手动 `jobs run` 可执行 |
| run_on_start | 启动后立即各触发一次 |
| busy | 上一次未结束时跳过并记 `job_busy` |
| last/next | 触发后更新；重启后重新计算 |
| 关闭 | Stop 取消 ticker、任务、生成与 Embedding |
| 双重 Stop | 第二次直接返回，不 panic |

#### 15.2.11 图片

| 用例 | 可观察断言 |
|---|---|
| 关闭态 | `vision_enabled=false` 时不发起图片请求；纯图片消息 `empty_text` |
| URL scheme | 非 http(s)、缺 host、带 userinfo 的图片 URL 被跳过 |
| 私网地址 | 回环、10/8、127/8、169.254/16、CGNAT、保留段等全部拒绝 |
| 重定向 | 每跳重新校验；超过 `vision_max_redirects=3` 失败并记 `reason=redirect` |
| 自定义 dialer | 仍拒绝回环/私网地址，不因宿主自定义 `DialContext` 绕过 |
| 状态码 | 非 200 跳过并记 `reason=status` |
| Content-Type | 非 `image/*` 时用 `DetectContentType` 兜底；仍非图片则跳过 |
| 大小限制 | 超过 4 MiB 跳过并记 `reason=too_large` |
| 格式白名单 | 不在允许集合的图片跳过并记 `reason=format` |
| 超时 | 批量下载共用 10s；ctx 取消优先 |
| 编码 | 成功图片编码为 `data:<mime>;base64,...`，不保留原始 URL |
| 数量上限 | 每批最多 4 张成功图片；超出的保留占位符 |
| `KeyFile` | 不下载、不进入 `Images` |
| 日志 | 只记 `reason`/`host`/`bytes`，不记完整 URL 或 base64 |
| debug 折叠 | `debug_prompts=true` 时图片只记 `[image <mime> <bytes>B]`，不含 base64 |

### 15.3 测试注入缝

| 注入点 | 类型 | 用途 |
|---|---|---|
| `now` | `func() time.Time` | 衰减、TTL、冷却、静默时段 |
| `randFloat` | `func() float64` | 概率边界 |
| `chatCompleter` | 接口 | 固定回复、错误、超时、非法 JSON、多模态请求断言 |
| `embedder` | 接口 | 固定维度向量、维度错误、零向量 |
| `visionClient` | `*http.Client` | 图片下载；测试替换为 `httptest.Server.Client()` 绕过地址限制 |
| `knowledgeFetcher` | 接口 | URL 校验、重定向、Content-Type、大小 |
| `Storage` | 接口 | 内存 fake、写失败、读超时 |
| `Tables` | 接口 | 表 fake：`CreateTable`/`ListRows` 错误、`bot.ErrRowExists`、`bot.ErrTableNotFound`、行读写失败 |
| `Plugin.vectorLoadWorkers` | int | 默认 4，测试设为 1；实例字段，非包级可变状态 |
| `Plugin.vectorReadyTimeout` | duration | 默认 2s，测试缩短 |
| `Plugin.tableLoadTimeout` | duration | 默认 30s，测试缩短 |

约束：

- 全部桩手写，只用标准库 `testing`。
- 不引入 `testify`、`gomock`、`goleak` 等第三方库。
- 桩必须可并发安全；竞态测试直接使用真实锁。
- 时间相关测试不使用 `time.Sleep` 猜时间，使用注入的 `now` 与可控 timer。

### 15.4 mock 适配器端到端

仓库内实现：`e2e_test.go` 是 `package chatagent`，直接调用 `kei.Run` 装配真实宿主、插件与 `adapters/mock`；不包含单独的 `main` 包，也不包含 SQLite 持久化场景。存储使用并发安全的 `memStore`（同时实现 `Storage` 与 `TableStorage`）。Chat/Embedding 由本地 `httptest.Server` 桩提供。

测试程序要点：

- 注册测试用 mock 适配器，通过 `kei.Run` 启动宿主；等待插件索引 `ready`。
- 配置 `group_policy=whitelist`、`group_list=[g9]`、`private_policy=open`、`self_ids=[bot]`、`super_admins=[e2e:admin]`、`memory_enabled=true`、`rag_enabled=true`、`vision_enabled=true`。
- 图片 e2e 显式把 `visionClient` 替换为本地 `httptest` client（生产受限 client 会拒绝回环地址）。
- 预置记忆与知识通过管理员命令，等待异步完成消息。

已覆盖场景：

1. 群聊名单与私聊目标：g1 无发送；g9 @ 回复；私聊 `Target.Kind=MessagePrivate`、`UserID` 正确、`ChannelID` 为空。
2. RAG 注入：检查 Chat 桩请求包含 `<long_term_memory>` 与 `<knowledge_base>`。
3. 整理与重启持久化：执行 `/chat jobs run consolidate`，断言 `vector` 表出现 `m:`/`meta`；重启宿主后 `/chat memory status` 记录仍在。
4. 知识库：`/chat knowledge add-text` 后断言 `d:` 与 `c:` 行及 `knowledge list` 计数。
5. 清理保护：`history_ttl` 到期但未整理会话仍受硬 TTL 保护。
6. 管理员：super_admin 覆盖持久化、核心管理员、非管理员拒绝。
7. 图片识别与记忆：Chat 桩收到 data URL parts，整理后写入识别文本，重启后可异步检索。

断言要求：

- 每条断言检查实际发送消息、KV 键、`vector` 表行或桩请求体，不检查“没有报错”。
- Chat/Embedding 响应与向量固定；时间敏感场景尽量轮询状态，现有少量 `time.Sleep` 仅用于负向/清理场景。
- 不依赖外部网络与真实 API key。
- SQLite/MySQL 后端不在当前 e2e 覆盖范围内，由库测试替换 `memStore` 验证核心语义。

### 15.5 竞态与优雅关闭

- 全部测试在 `go test -race ./...` 下通过。
- 关闭场景：
  - 生成中调用 `Stop`：不再发送，`wg` 在 5s 内归零。
  - Embedding 中调用 `Stop`：请求 ctx 取消，任务返回。
  - 知识库抓取中调用 `Stop`：抓取 ctx 取消，不写半成品。
  - 图片下载中调用 `Stop`：下载 ctx 取消，不发送、不写记忆，`wg` 归零。
  - 写队列有积压时 `Stop`：在剩余 `shutdown_wait_timeout` 内排空后取消，已提交写入保留；排空超时返回 `关闭不完整`。
  - 索引异步构建中调用 `Stop`：构建 ctx 取消，`wg` 归零，索引关闭后不进入 `ready`，不发送消息。
  - `Stop` 后调用 `Send` 或 Handler/timer 新任务的路径必须被拒绝，测试断言发送计数不增加。
  - 连续两次 `Stop`：第二次立即返回，不 panic；先 `Stop` 再 `Start` 返回 `chat-agent: 插件已关闭`。
- 不引入 `goleak`；用显式 `sync.WaitGroup`、原子计数与 `Stop` 返回时限验证无泄漏；写协程纳入 `wg`。
- timer 回调在 `Stop` 后不得触发发送；`epoch` 与 timer 布防序号共同校验，旧回调不得清除新 timer。
- 记忆相关任务（consolidate/memory/reindex）互斥；行写入后用版本 CAS 切换内存快照，删除先写 tombstone；不在锁内做 Storage 调用。
- 并发测试至少覆盖：同群不同用户并发 Handler、生成与清理并发、整理与知识库并发写队列、timer 重布防竞态。

### 15.6 文档校验

可执行的检查（仓库根）：

```bash
# 文档校验测试（章号、配置键、固定字面量）
go test -run 'TestDocs|TestMetadataOptions' ./...

# 章号 1-16 连续
grep -h '^## [0-9]\+\.' docs/*.md | sed -E 's/^## ([0-9]+)\..*/\1/' | sort -n

# 配置键集合与 Metadata.Options 一致
perl -ne 'print "$1\n" if /^\| `([a-z0-9_]+)` \|/' docs/configuration.md | sort > /tmp/keys
perl -0777 -ne 'if (/Options:\s*\[\]string\s*\{(.*?)\}/s) { $b=$1; while ($b =~ /"([a-z0-9_]+)"/g) { print "$1\n" } }' docs/architecture.md | sort > /tmp/options
diff -u /tmp/keys /tmp/options

# 固定字面量
grep -RIn "你是 {{bot_name}}" docs
grep -RIn "你是记忆整理器" docs
grep -RIn "empty_text, no_sender" docs
grep -RIn "chat-agent: 无权执行该命令" docs
grep -RIn "plugin:chat-agent:" docs
grep -RIn "TableStorage" docs
grep -RIn "table_load_timeout=30s" docs
grep -RIn "m:<id32>" docs
grep -RIn "d:<id32>" docs
grep -RIn "c:<id32>" docs
grep -RIn '"version":1,"model":"...","dims":1536' docs
grep -RIn "chat-agent: 需要 TableStorage 支持" docs
grep -RIn "chat-agent: 存储表初始化失败" docs
grep -RIn "chat-agent: 存储表加载失败" docs
```

负向检查（旧索引清单键与旧 KV 记忆/知识记录前缀必须零命中）只在 Verifier 侧执行，不写进 `docs/`，避免文档自身命中。

补充检查：

- `docs/README.md` 列出每个 `docs/*.md` 文件名；`docs_test.go` 检查文件名出现与章号 1-16 唯一。
- `docs/*.md` 中的 `*.md` 链接目标存在，`#anchor` 能在目标文件标题中找到；根 `README.md` 与 `AGENTS.md` 链接由人工检查。
- [`configuration.md §13.1`](configuration.md#131-权威配置键表)、§13.6、[`architecture.md §6`](architecture.md#6-与-kei-核心的契约对应) 三处键集合一致，共 82 项；`docs_test.go` 逐项比较。
- 每个 `docs/*.md` 含“覆盖：”与“相关文档”段落。
- `configuration.md` 默认值、表名/行键/JSON 形状由人工/契约测试覆盖，不在 `docs_test.go` 全量解析。
- `docs/` 与 `go.mod`、`.go` 源码、测试共存；文档校验只约束文档内容与键集合。

### 15.7 完成定义

- 质量门全部通过。
- 15.2 矩阵全部有用例并通过。
- 15.4 端到端场景可复现通过。
- 15.5 竞态与关闭场景通过。
- 15.6 文档校验通过。
- 新增或修改的配置键、命令输出、固定字面量与文档一致。

## 相关文档

- 配置键：[`configuration.md §13.1`](configuration.md#131-权威配置键表)。
- 命令输出：[`admin.md §14.3`](admin.md#143-子命令与输出)。
- 决策 reason：[`participation.md §7.7`](participation.md#77-决策日志与-reason-词表)。
- 向量与容量：[`vector-index.md`](vector-index.md) 第 11 章。
- 交付阶段：[`roadmap.md §16.2`](roadmap.md#162-实现阶段)。
