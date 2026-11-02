# 参与决策

覆盖：一条消息从进入 Handler 到被拒绝、记录、寻址、随机选中、批处理、生成与发送的完整判定。不覆盖：会话存储字段、记忆/知识库检索细节、命令输出全文，这些见 [`conversation.md`](conversation.md)、[`memory.md`](memory.md)、[`rag.md`](rag.md)、[`admin.md`](admin.md)。

## 7. 参与决策

### 7.1 事件过滤与准入

Handler 按以下顺序判定，任一条件命中立即返回并记录 reason。顺序不可交换。

| 顺序 | 条件 | reason | 副作用 |
|---|---|---|---|
| 1 | 事件没有可渲染文本段，且没有可用图片段（纯表情/文件；或 `vision_enabled=false` 时的纯图片） | `empty_text` | 不记录、不建状态 |
| 2 | `Sender == nil` | `no_sender` | 不记录、不建状态 |
| 3 | `Sender.IsBot && ignore_bots` | `bot_sender` | 不记录、不建状态 |
| 4 | `Sender.ID` 在 `self_ids` 中 | `self_sender` | 不记录、不建状态 |
| 5 | `ev.Command != nil && !respond_to_commands` | `command` | 不记录、不建状态 |
| 6 | 名单策略拒绝 | `not_allowed` | 不新建状态、不追加历史 |
| 7 | 去首尾空白后的文本 rune 数 `< trigger_min_chars`，且没有可用图片（`vision_enabled=true`） | `too_short` | 不记录、不建状态 |
| 8 | 会话覆盖 `disabled=true` | `channel_off` | 不记录 |
| 9 | 通过以上全部 | — | 记录历史、更新活跃时间与参与者 |

补充规则：

- 只有包含文本段的消息进入历史；`vision_enabled=true` 时，含至少一个 `SegImage` 且 `KeyURL` 非空的消息也进入历史。含文本的混合消息把非文本段渲染为占位符；纯图片消息的 `Text` 为 `[图片]` 并保留图片 URL；纯表情/文件仍按 `empty_text` 处理。
- 图片只接受 `SegImage` 的 `KeyURL`；`KeyFile`（本地路径或平台文件标识）不支持，视为不可用图片。`KeyURL` 的 scheme、地址限制、大小与格式在后台下载时校验；Handler 只检查非空。含图片的消息跳过 `trigger_min_chars` 文本长度检查。
- `/chat` 与 `/chat-admin` 由独立命令规则处理，不进入消息 Handler，因此不记录历史、不调用聊天 LLM，也不受名单与会话开关影响。
- `respond_to_commands` 只控制“其它命令消息是否作为普通聊天消息处理”。为 `false` 时一律跳过且不进历史；为 `true` 时按普通消息继续判定，可能进入历史。
- 名单拒绝不新建会话状态；已有会话状态不追加历史，但保留原有历史与计数。
- `self_ids` 匹配 `Sender.ID` 原值；机器人自身发送的消息只要到达 Handler 就必须被 `self_sender` 拦住，避免自问自答。

### 7.2 寻址判定

| 条件 | 群聊 | 私聊 |
|---|---|---|
| 始终寻址 | 否 | 是 |
| `SegAt` 的 `KeyUserID` 在 `self_ids` 中 | 是 | 是（私聊已恒寻址，规则不额外生效） |
| `self_ids` 为空时任意 `SegAt` | 是，但 `KeyUserID == "all"` 永远不算 | 同上 |
| `SegReply.KeyMessageID` 在最近 128 条机器人发送 ID 环中 | 是 | 是 |
| 文本包含任一 `trigger_keywords` | 是 | 是 |
| 以上都不满足 | 否 | 不适用 |

规则细节：

- `self_ids` 非空时，只有 `KeyUserID` 精确匹配才因 @ 寻址；`all` 不匹配，且无论 `self_ids` 是否为空，`KeyUserID == "all"` 都不算寻址。
- 引用寻址依赖成功发送后记录的消息 ID；适配器未返回消息 ID 时该条不进入环，引用该条不会触发寻址。
- 关键词是子串匹配：ASCII 字母大小写不敏感，中文按原样子串匹配；`trigger_keywords` 为空表示没有关键词寻址。
- `trigger_min_chars` 在寻址前生效，过短消息不会因 @、引用或关键词进入生成。
- 私聊恒寻址，但仍受 `mention_reply_probability` 与 `mention_min_interval` 控制。

### 7.3 随机参与

随机路径只用于群聊的非寻址消息；私聊不使用随机路径，寻址消息也不使用随机路径。

每个候选批次按以下顺序判定，首个失败条件即返回对应 reason：

| 顺序 | 条件 | 失败 reason | 说明 |
|---|---|---|---|
| 1 | `random_enabled` | `not_addressed` | 关闭时非寻址消息永不触发 |
| 2 | `random_max_per_hour == 0` | `hour_quota` | 0 表示随机插话关闭 |
| 3 | 距 `lastReplyAt` 不足 `random_cooldown` | `cooldown` | 冷却针对最近一次机器人回复 |
| 4 | 当前时间落在 `random_quiet_hours` | `quiet_hours` | 仅在 `random_timezone` 下解释 |
| 5 | 最近 1 小时随机插话数 `>= random_max_per_hour` | `hour_quota` | 只统计随机插话 |
| 6 | 最近 `random_activity_window` 内不同非机器人发送者数 `< random_min_participants` | `min_participants` | 0 表示不限制 |
| 7 | 掷 `random_probability` 未命中 | `probability` | 每个候选批次只掷一次 |

说明：

- 非寻址消息到达时，若当前没有待处理批次、pending 或在途生成，评估一次随机路径；通过则创建批次并把该消息纳入。窗口内后续命中消息直接加入同一批次，不再掷概率。
- 会话已有待处理批次时，新的命中消息加入该批次；非寻址消息只记录历史，不重新评估随机路径。
- 寻址消息加入随机批次时，该批 `pendingRandom` 置为 false，整批按寻址轮次处理（不识别 skip token）。
- 已有在途生成时，非寻址消息只记录历史，不评估随机路径，也不进入 pending。
- `random_quiet_hours` 为空串或 `start == end` 表示禁用；支持跨零点区间，例如 `23:00-07:00`。区间边界包含开始、不包含结束。
- `random_probability=0` 永不触发；`=1` 在其它条件全部满足时必定触发。
- 随机轮次允许模型用 `llm_skip_token` 拒答；`llm_skip_enabled=false` 时忽略该 token。寻址轮次不接受 skip token。

被寻址消息与私聊的回复控制：

| 顺序 | 条件 | 失败 reason |
|---|---|---|
| 1 | 距 `lastReplyAt` 不足 `mention_min_interval` | `cooldown` |
| 2 | 掷 `mention_reply_probability` 未命中 | `probability` |
| 3 | 通过 | 进入批处理 |

寻址/私聊不检查静默时段、小时配额与参与人数；这些只约束随机插话。

### 7.4 批处理

- 首条进入批处理的消息设置 `firstAt`，布防 `batch_window`。
- 后续进入批处理的消息把定时器重置为 `min(batch_window, firstAt + batch_max_window - now)`；到达 `firstAt + batch_max_window` 必须触发。
- 定时器触发时整体取走 `pending`；同一会话同时最多一个生成。
- 生成期间到达的命中消息不丢弃，直接追加到 `pending`，不布防新 timer；各自历史记录与序号已保留。
- 定时器触发时若已有在途生成，保留 `pending` 并直接返回，不重复入队。
- 生成完成后若有 `pending`，100ms 后立即取走并开始下一批。
- 全局信号量满时丢弃本批，`reason=semaphore_full`，不排队。
- 取批时校验会话 `epoch`；epoch 已变化则丢弃并记 `stale`。

时间线示例（`batch_window=2500ms`，`batch_max_window=8s`）：

```text
t=0.0s  消息 A：firstAt=0，timer 到 2.5s
t=1.5s  消息 B：timer 重置到 4.0s
t=3.5s  消息 C：timer 重置到 6.0s
t=7.0s  消息 D：min(2.5s, 8.0-7.0=1.0s)，timer 到 8.0s
t=8.0s  触发，批次 = [A, B, C, D]
```

批内消息按 `seq` 升序排列；`batch.firstSeq` 是本批第一条的序号，用于从历史块中排除本轮消息。

### 7.5 生成与发送

生成协程按以下顺序执行：

1. 获取全局信号量；满则记 `semaphore_full`，结束。
2. 校验会话 `epoch`；不匹配记 `stale`，结束。
3. 快照本批消息与历史；历史只取 `seq < batch.firstSeq` 的条目。
4. 调用 RAG 检索并渲染记忆/知识库块；查询文本只含文本，不含图片；失败不阻塞聊天，记 `rag_status=error`。`vision_enabled=true` 时同时选择、下载并组装图片（见 [`conversation.md §8.3`](conversation.md#83-上下文渲染)）；单图失败只保留 `[图片]` 占位符。
5. 调用 Chat Completions；有图片时 `user.content` 为多模态 parts，否则为字符串；失败记 `llm_error`。
6. 清洗回复：
   - CRLF 转 LF。
   - 去掉首尾空白。
   - 仅当本批是随机插话且 `llm_skip_enabled=true` 时，去掉 `llm_skip_token` 的独立出现；移除后为空则记 `skipped_by_llm`，不发送。寻址与私聊轮次不识别该 token。
   - 连续 3 个以上换行压成 2 个。
   - 其余情况清洗后为空记 `empty_reply`。
7. 去重：与最近 10 分钟内最近 5 条机器人回复比较规范化文本；相同则记 `duplicate_reply`，不发送。规范化文本指去首尾空白、内部连续空白压成单个空格、ASCII 字母转小写。
8. 截断：超过 `reply_max_chars` 个 rune 时截断到该长度，并把最后一个 rune 替换为 `…`；结果长度不超过 `reply_max_chars`。
9. 构造消息段：
   - `reply_quote=true` 且本批最后一条有消息 ID 时，附加引用该消息的段；没有消息 ID 时省略引用。
   - `reply_mention_sender=true` 且为群聊时，在开头附加 `At` 最后一条消息的发送者。
10. 构造 Target：
    - 群聊：`Kind=MessageGroup`，填 `ChannelID`，`UserID` 留空。
    - 私聊：`Kind=MessagePrivate`，填 `UserID`，`ChannelID` 留空。
11. 调用 `BotAPI.Send`；失败记 `send_error`，不重试、不重排。
12. 发送成功后把机器人回复写入历史：`IsSelf=true`、`UserName=bot_name`、`seq` 递增；更新 `lastActivity`、`lastReplyAt`、`recentReplyIDs`、`recentReplyTexts`；随机插话同时写入 `hourly`。
13. 若存在 `pending`，100ms 后触发下一批。

`BotAPI.Send` 未返回消息 ID 时，该条不加入 `recentReplyIDs`，引用寻址与引用发送对该条不可用；回复文本去重仍然生效。

图片失败语义：

- 图片 URL 非法、下载失败、超时、非图片、格式不允许或超过 4 MiB 时，该图不发送给 LLM，文本中的 `[图片]` 占位符保留；记 `operation=vision reason=<bad_url|fetch_error|status|redirect|too_large|not_image|format|limit>`，不进入决策 reason 词表。
- 单批最多附加 4 张成功下载的图片；超出的图片保留占位符，日志记 `reason=limit`。
- `vision_enabled=true` 但 `llm_model` 不支持多模态 `content` 时，Chat 请求返回 4xx，生成记 `llm_error`，不自动回退为纯文本重试。
- 图片下载失败不阻止文本消息生成；纯图片消息在全部图片失败时仍按 `Text=[图片]` 参与生成。

### 7.6 决策伪代码

```text
func handleMessage(ev, kind):
    if not hasContent(ev):              return log(empty_text)  // 文本或可用图片
    if ev.Sender == nil:                return log(no_sender)
    if ev.Sender.IsBot && ignore_bots:  return log(bot_sender)
    if ev.Sender.ID in self_ids:        return log(self_sender)
    if ev.Command != nil && !respond_to_commands:
                                        return log(command)
    if not policy.allows(kind, ev):     return log(not_allowed)
    if runeLen(trimSpace(text(ev))) < trigger_min_chars && not usable_image(ev):
                                        return log(too_short)
    session = sessions.getOrCreate(ev)
    if session.disabled:                return log(channel_off)

    turns = session.appendHistory(ev)   // 文本 + 可选图片 URL
    session.touch(ev)

    addressed = kind == private || isAt(ev) || isQuote(ev) || hasKeyword(text(ev))
    if not addressed:
        if session.inflight || session.timer != nil || len(session.pending) > 0: return
        if reason = random.reject(session): return log(reason)
        session.startBatch(turns, random=true)
        return

    if reason = mention.reject(session): return log(reason)
    if session.inflight:
        session.pending = append(session.pending, turns...)
        return
    session.startBatch(turns, random=false)

func startBatch(turns, random):
    if session.timer == nil:
        session.firstAt = now()
    session.pending = append(session.pending, turns...)
    session.resetTimer(min(batch_window, firstAt+batch_max_window-now()))

func onBatchTimer(session, epoch):
    if session.epoch != epoch: return log(stale)
    if session.inflight: return
    batch = session.takePending()
    if len(batch) == 0: return
    if not semaphore.tryAcquire():
        return log(semaphore_full)
    session.inflight = true
    go generate(session, batch, epoch)

func generate(session, batch, epoch):
    defer semaphore.release()
    defer session.finishGeneration()
    if session.epoch != epoch: return log(stale)
    hist = session.historyBefore(batch.firstSeq)
    images = vision.fetch(hist, batch)   // 仅 vision_enabled
    rag = rag.retrieve(text(batch))
    req = prompt.build(hist, batch, rag, images, session)
    reply, err = llm.complete(req)
    if err != nil: return log(llm_error)
    reply = clean(reply)
    if reply == "": return log(empty_reply or skipped_by_llm)
    if session.isDuplicate(reply): return log(duplicate_reply)
    reply = truncate(reply, reply_max_chars)
    msg = buildMessage(reply, batch, session)
    if err = bot.Send(ctx, target(session), msg); err != nil:
        return log(send_error)
    session.appendSelfReply(reply, msg.ID, batch.random)
    scheduleNextIfPending(session, 100ms)
```

### 7.7 决策日志与 reason 词表

reason 逐字固定为：

```text
empty_text, no_sender, bot_sender, self_sender, command, not_allowed, too_short,
channel_off, not_addressed, probability, cooldown, quiet_hours, hour_quota,
min_participants, semaphore_full, llm_error, skipped_by_llm, empty_reply,
duplicate_reply, send_error, stale
```

裁剪历史时另有一条 `operation=message reason=history_trimmed` 的 warn 日志；它不进入上面的决策 reason 词表。

`rag_status` 单独记录，不进入 reason 词表：

| 值 | 含义 |
|---|---|
| `ok` | 至少一个库检索成功并注入 |
| `disabled` | 记忆与知识库都关闭，或对应预算为 0 |
| `loading` | 向量索引未就绪且等待 2s 后仍未完成 |
| `error` | Embedding 或检索失败 |
| `empty` | 检索成功但无结果 |

日志示例：

```text
operation=message reason=probability session=3f9a1c kind=group platform=telegram user=42 channel=-100123 duration_ms=0
operation=generate reason=skipped_by_llm rag_status=ok session=3f9a1c kind=group platform=telegram user=42 channel=-100123 duration_ms=812
```

禁止在日志中记录完整提示词、API key 或知识库正文；只有 `debug_prompts=true` 才记录提示词与响应。

### 7.8 名单策略

四种模式语义：

| 模式 | 群聊 | 私聊 |
|---|---|---|
| `off` | 全部拒绝 | 全部拒绝 |
| `open` | 全部放行 | 全部放行 |
| `whitelist` | 仅 `group_list` 命中放行 | 仅 `private_list` 命中放行 |
| `blacklist` | `group_list` 命中拒绝，其余放行 | `private_list` 命中拒绝，其余放行 |

作用对象：

- 群聊按 `Channel.ID` 匹配。
- 私聊按 `Sender.ID` 匹配。

条目匹配：

- `platform:id` 形式精确匹配平台名与 ID，例如 `telegram:-100123`。
- 不含冒号的条目匹配任意平台的原始 ID，例如 `-100123`。
- 配置校验要求条目非空；`group_list`/`private_list` 的 `platform:id` 两侧都不能为空。加载时平台条目会 trim 两侧空白；运行时 `list add` 同样校验并规范化。
- 列表去重并保持首次出现顺序。

加载与覆盖：

- 未覆盖时使用配置值：`group_policy`、`private_policy`、`group_list`、`private_list`；默认值由 [`configuration.md §13.1`](configuration.md#131-权威配置键表) 定义。
- `policy` Storage 键存在时，按 `group_mode`、`group_list`、`private_mode`、`private_list` 逐字段覆盖配置；`[]` 覆盖为空名单。
- JSON 整体损坏时整个覆盖回落到配置并 warn；单个字段（模式或列表）非法时只回落该字段，其余合法字段继续生效。
- `/chat policy reset` 删除 `policy` 键，恢复配置值。

运行时修改：

- `/chat policy <group|private> <mode>` 修改模式并写穿透。
- `/chat list <group|private> add|del <id>` 修改列表并写穿透。
- 输出字面量见 [`admin.md §14.3`](admin.md#143-子命令与输出)。

绕过规则：

- `/chat` 与 `/chat-admin` 命令不受名单策略、会话开关、`trigger_min_chars`、批处理影响，否则管理员无法恢复被关闭的会话。
- 被名单拒绝的消息不记录历史、不更新活跃时间、不参与随机参与人数。
- 多平台部署且存在裸 ID 条目时，裸 ID 会在所有平台匹配；跨平台隔离必须使用 `platform:id`。该限制同时记录在 [`roadmap.md §16.3`](roadmap.md#163-已知缺口)。

## 相关文档

- 会话状态与历史：[`conversation.md`](conversation.md) 第 8 章。
- 提示词与上下文：[`conversation.md §8.3`](conversation.md#83-上下文渲染)。
- RAG 检索与注入：[`memory.md §9.5-9.6`](memory.md#95-检索)、[`rag.md §10.6-10.7`](rag.md#106-检索与融合)。
- 命令输出：[`admin.md §14.3`](admin.md#143-子命令与输出)。
- 配置键：[`configuration.md §13.1`](configuration.md#131-权威配置键表)。
