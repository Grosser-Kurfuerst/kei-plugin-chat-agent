# LLM 与 Embedding 客户端

覆盖：Chat Completions 与 Embeddings 的请求/响应、超时与重试、长度与成本、安全与日志、兼容服务、可注入接口。不覆盖：消息组装（见 [`conversation.md §8.3`](conversation.md#83-上下文渲染)）、抽取提示词（见 [`memory.md §9.2`](memory.md#92-抽取)）、向量索引（见 [`vector-index.md`](vector-index.md) 第 11 章）。

## 12. LLM 与 Embedding 客户端

### 12.1 Chat Completions

请求：

```http
POST <llm_base_url>/chat/completions
Authorization: Bearer <llm_api_key>
Content-Type: application/json
<extra headers>

{"model":"<llm_model>","messages":[...],"temperature":<llm_temperature>,"max_tokens":<llm_max_tokens>,"stream":false}
```

- `llm_base_url` 规范化：去掉末尾 `/`，再拼 `/chat/completions`。
- `llm_api_key` 为空时不发送 `Authorization` 头，供本地服务使用。
- `messages` 由 [`conversation.md §8.3`](conversation.md#83-上下文渲染) 构造：`system` 一条、`user` 一条。
- `messages[0].content` 始终是字符串；`messages[1].content` 在 `vision_enabled=true` 且有可用图片时是 `[]ContentPart`，否则是字符串。`ContentPart` 只允许 `text` 与 `image_url` 两种类型，格式见 [`conversation.md §8.3`](conversation.md#83-上下文渲染)。
- `image_url.url` 是 `data:<mime>;base64,...`；插件不把原始图片 URL 发给模型。单次请求最多 `vision_max_images_per_request=4` 张成功下载的图片，单图不超过 `vision_max_image_bytes=4194304` 字节。`llm_model` 必须支持该格式；插件不探测模型能力。
- `stream` 固定 `false`，不支持流式响应。
- `llm_extra_headers` 逐项加入；禁止覆盖 `authorization`、`content-type`、`content-length`、`host`（大小写不敏感）。
- 只接受 HTTP 200。

响应解析：

- 响应体必须是 JSON 对象。
- `choices` 必须是非空数组；缺失或为空报错。
- 取 `choices[0].message.content`。
- `content` 必须是字符串；数组、对象、数字、null 都视为错误。
- 空字符串是错误；只有空白字符的字符串在清洗阶段按 `empty_reply` 处理。
- 不使用 `choices[0].finish_reason` 做业务判断；`length` 只记日志。
- 不解析工具调用、多模态响应或流式分片；响应 `content` 只接受字符串。

`[SKIP]` 语义：

- 只在随机插话轮次且 `llm_skip_enabled=true` 时生效。
- 清洗后文本等于 `llm_skip_token`，或删除 token 后为空，视为 `skipped_by_llm`，不发送。
- 寻址轮次与私聊轮次不识别 skip token，按普通文本处理。

### 12.2 Embeddings

请求：

```http
POST <embedding_base_url>/embeddings
Authorization: Bearer <embedding_api_key>
Content-Type: application/json
<extra headers>

{"model":"<embedding_model>","input":["..."],"dimensions":<embedding_dimensions>}
```

- `embedding_base_url` 为空时回落 `llm_base_url`；否则必须通过 URL 校验。
- `embedding_api_key` 为空时回落 `llm_api_key`。
- `dimensions` 只在 `embedding_dimensions > 0` 时发送；为 0 时省略，由服务端决定。
- `input` 是字符串数组；单条截断到 `embedding_max_input_chars=6000` rune；批大小 `embedding_batch_size=16`。
- Embeddings 只接收文本，不接收图片。`vision_enabled=true` 时图片先由 Chat LLM 识别为文本，再把识别文本嵌入；`embedding_model` 不需要图片输入，插件不向 `/embeddings` 发送 `image_url` 或 data URL。
- 空输入直接返回空结果，不发起请求。
- `embedding_extra_headers` 的禁改列表与 Chat 相同。
- 只接受 HTTP 200。

响应校验：

1. 响应体是 JSON 对象，`data` 是数组。
2. 每项有整数 `index` 与数组 `embedding`。
3. 按 `index` 升序排序；`index` 必须恰好覆盖 `0..len(input)-1`，不重不漏。
4. 每个分量的数值必须有限（拒绝 NaN/Inf）。
5. 每个向量范数必须大于 0；零向量拒绝。
6. 所有向量维度一致。
7. `embedding_dimensions > 0` 时每个向量维度必须等于该值。
8. `embedding_dimensions == 0` 时以首个向量维度为准，其余必须一致。

任一校验失败整批失败；不返回部分结果。归一化由向量索引层执行，客户端保留原始浮点值。

### 12.3 超时、重试与错误

- 每次尝试使用带超时的 ctx：Chat 用 `llm_timeout`，Embedding 用 `embedding_timeout`。
- 最多尝试 `1 + max_retries` 次；`llm_max_retries` 与 `embedding_max_retries` 分别控制。
- 可重试：HTTP 429、HTTP 5xx、连接失败、读写超时。
- 不重试：HTTP 4xx（429 除外）、JSON 解析失败、响应结构非法、维度/数值校验失败、ctx 被取消。
- 退避固定为 1s、2s、4s，之后每次 30s 上限；不引入随机抖动。
- 取消优先：ctx 已取消时立即返回，不进入下一次尝试。
- 错误响应体最多读取 500 rune，替换控制字符后写入日志。
- 错误分类：

| 分类 | 触发 |
|---|---|
| `timeout` | 单次尝试超时或整体 ctx 超时 |
| `canceled` | ctx 被取消 |
| `http_429` | 限流 |
| `http_4xx` | 其它客户端错误 |
| `http_5xx` | 服务端错误 |
| `transport` | 连接、TLS、读写失败 |
| `invalid_json` | 响应不是合法 JSON |
| `invalid_shape` | 缺字段、类型错误、数量/顺序/维度不符 |
| `invalid_vector` | NaN/Inf、零向量 |

错误只返回，不 panic。Chat 错误映射为决策 reason `llm_error`；Embedding 错误在 RAG 中映射为 `rag_status=error`，在整理/知识库任务中映射为 `embedding_error`。

### 12.4 长度与成本

- 聊天回复上限 `llm_max_tokens`；提示词预算由历史、记忆、知识库三部分控制。
- 历史条数 `context_max_messages`、历史字符 `context_max_chars`。
- 记忆块 `memory_context_max_chars`、知识库块 `knowledge_context_max_chars`、总预算 `rag_context_max_chars`。
- 记忆抽取固定 `max_tokens=800`、`temperature=0.2`，单条消息截断 1000 rune。
- Embedding 批大小 16，单条输入 6000 rune；知识库分块目标 800 rune。
- 超预算截断顺序见 [`conversation.md §8.3`](conversation.md#83-上下文渲染) 与 [`rag.md §10.6`](rag.md#106-检索与融合)。
- 成本来源：每条聊天回复一次 Chat 调用；每条随机回复可能因 skip 产生一次无效调用；整理与 reindex 产生 Embedding 调用。`vision_enabled=true` 时图片作为多模态输入计入 Chat 令牌；单次最多 4 张，图片下载不产生 Embedding 调用。文档不承诺成本上限。

### 12.5 安全与日志

- API key 只放在 `Authorization` 头，不写入日志、错误消息或 Storage。
- `llm_extra_headers`/`embedding_extra_headers` 禁止覆盖协议头；校验失败输出 `不得覆盖协议请求头`。
- 默认只记录请求方法、URL 的 scheme/host/path、状态码、耗时、错误分类；不记录请求体、响应体、提示词。
- `debug_prompts=true` 时以 Debug 级别记录完整请求与响应；这是唯一记录提示词正文的场景。图片 `image_url.url` 折叠为 `[image <mime> <bytes>B]`，不输出 base64；原始图片 URL 也不记录。
- 错误体与摘要最多 500 rune。
- URL 必须是带 host 的 http(s)，不得包含 userinfo。
- 使用宿主 `HTTPClient`，不关闭 TLS 校验，不设置 `InsecureSkipVerify`。
- 不在日志中输出知识库正文、记忆文本或用户消息全文。

### 12.6 兼容服务

| 服务 | `llm_base_url` | Chat 模型示例 | Embedding 模型示例 |
|---|---|---|---|
| OpenAI | `https://api.openai.com/v1` | `gpt-4o-mini` | `text-embedding-3-small` |
| DeepSeek | `https://api.deepseek.com/v1` | `deepseek-chat` | 需单独配置实现 `/embeddings` 的兼容服务 |
| Ollama | `http://127.0.0.1:11434/v1` | `llama3.1` | `nomic-embed-text` |
| vLLM | `http://127.0.0.1:8000/v1` | 部署时的模型名 | 部署时的 Embedding 模型名 |

要求：

- 服务必须实现 OpenAI 兼容的 `/chat/completions` 与 `/embeddings`。
- 只支持 JSON 响应，不支持流式、工具调用、多模态响应。
- `vision_enabled=true` 时 Chat 服务必须接受 OpenAI 多模态 `content` 数组（`text` 与 `image_url` 块）；只支持文本的模型在含图请求返回 4xx，映射为 `llm_error`。
- Embeddings 服务仍只需文本输入；不需要多模态 Embeddings 协议。
- `embedding_base_url` 与 `embedding_api_key` 可以指向与 Chat 不同的服务。
- 本地服务可以省略 API key，但必须提供可访问的 base URL。

### 12.7 客户端接口

```go
type chatCompleter interface {
    Complete(ctx context.Context, req ChatRequest) (ChatResult, error)
}

type embedder interface {
    Embed(ctx context.Context, inputs []string) ([][]float32, error)
}

type ChatRequest struct {
    Model       string
    Messages    []ChatMessage
    Temperature float64
    MaxTokens   int
    ExtraHeaders map[string]string
}

type ChatMessage struct {
    Role    string
    Content any // string 或 []ContentPart
}

type ContentPart struct {
    Type     string    // "text" | "image_url"
    Text     string    // Type=="text" 时使用
    ImageURL *ImageURL // Type=="image_url" 时使用
}

type ImageURL struct {
    URL string // data:<mime>;base64,...
}

type ChatResult struct {
    Content      string
    FinishReason string
}
```

约定：

- `Embed` 返回顺序与 `inputs` 一致；任何一条失败整批返回错误。
- `Complete` 返回的 `Content` 已做类型校验，未做业务清洗。
- `ChatMessage.Content` 为字符串或 `[]ContentPart`；`ChatResult.Content` 始终是字符串。只有 `vision_enabled=true` 且图片可用时才构造 parts。
- 两个接口都不 panic；错误带分类，供调用方映射 reason。
- 测试用固定桩替换接口；不引入第三方 mock 库。
- 生产实现只依赖标准库 `net/http` 与 `encoding/json`。

## 相关文档

- 消息与提示词组装：[`conversation.md §8.3`](conversation.md#83-上下文渲染)。
- 记忆抽取：[`memory.md §9.2`](memory.md#92-抽取)。
- Embedding 校验与索引：[`vector-index.md §11.2`](vector-index.md#112-数据结构)、§11.6。
- 配置键：[`configuration.md §13.1`](configuration.md#131-权威配置键表)。
- 测试矩阵：[`testing.md §15.2`](testing.md#152-单元测试矩阵)。
