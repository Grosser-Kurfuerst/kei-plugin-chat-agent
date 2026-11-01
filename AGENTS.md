# AGENTS.md

本文件是本仓库的代理与贡献者规则。项目文档是验收基线；当前阶段只有文档，没有可运行代码。

## 1. 项目定位

- 插件名 `chat-agent`；Go module `github.com/RandomLemon/kei-plugin-chat-agent`；根包名 `chatagent`。
- kei 的进程内插件：处理群聊/私聊消息，提供名单控制、短期历史、长期记忆、知识库 RAG、两个定时任务与超级管理员命令。
- 宿主通过 `plugins.chat-agent.enabled: true` 启用，并在 main 包空导入本模块。
- 项目边界见 [`docs/architecture.md §1`](docs/architecture.md#1-项目定位与边界)；交付状态见 [`docs/roadmap.md §16.1`](docs/roadmap.md#161-现状)。

## 2. 硬性规则

### 2.1 分层与依赖

- 生产代码只依赖 `github.com/RandomLemon/kei/pkg/bot` 与 `github.com/RandomLemon/kei/pkg/message`。
- 禁止 import `github.com/RandomLemon/kei/internal/...`，也不得复制其实现。
- 测试可导入 `github.com/RandomLemon/kei/adapters/mock` 与 `github.com/RandomLemon/kei/pkg/kei` 做端到端宿主测试。
- 禁止平台名分支（如 `if platform == "telegram"`）；平台差异只使用 `bot` 包提供的原语。
- 配置与密钥只经 `PluginContext.Config` 读取；禁止读环境变量（含 `os.Getenv`）、禁止读本地文件。
- 禁止 Go `plugin` 机制；插件以编译期注册方式装配。
- 运行时零第三方依赖：标准库 + kei。

### 2.2 技术栈

- Go 1.25；标准库 `net/http` 调用 Chat Completions 与 Embeddings。
- 本地开发使用 `replace github.com/RandomLemon/kei => ../kei`；flake devShell 提供工具链。
- 不引入日志、配置、HTTP、mock、断言等第三方库。

### 2.3 运行时契约

- 所有阻塞操作必须带 `context.Context`；消息 Handler 必须毫秒级返回，网络与 LLM 调用放在后台协程。
- `PluginContext` 只在 `Setup` 捕获并挂在插件实例上；Handler 的 ctx 没有 `PluginContext`。
- `Start(ctx)` 的 ctx 在返回后立即取消；插件级 ctx 必须是 `context.WithCancel(context.WithoutCancel(ctx))`，由 `Stop` 取消。
- 所有 goroutine 必须能被 `Stop` 终止；等待使用实例持有的 `sync.WaitGroup`。
- 共享状态挂在插件实例上并加锁；禁止包级可变状态。
- 错误返回，不 panic；日志使用 `log/slog` 字段化输出。
- 时间一律 UTC；ID 一律字符串；Storage 逻辑键与 TableStorage 逻辑表名各不超过 173 字节，行键不超过 191 字节。
- `PluginContext.Tables` 与 `Storage` 共用 `storage` 权限，不新增权限；`memory_enabled || rag_enabled` 时 `Tables` 必须非 nil，否则 `Setup` 返回 `chat-agent: 需要 TableStorage 支持` 且插件不装配。
- 表行无列 schema、无 TTL、无跨行事务；记忆、知识库与向量索引只存逻辑表 `vector` 的 `m:<id32>`、`d:<id32>`、`c:<id32>`、`meta` 行，不使用索引清单或 KV 索引键。
- Storage KV 与表行写入经同一单写协程串行执行；`CreateTable`/`ListRows` 只在 Start 加载阶段直接调用，不经过写队列。`Stop` 先关闭写入准入并排空队列，再取消插件级 ctx。
- 消息处理不得阻塞事件总线；同一会话的并发由会话锁与批处理串行化。

### 2.4 当前阶段边界

- 本阶段只交付 `AGENTS.md` 与 `docs/*.md`。
- 不创建 `go.mod`、`.go` 文件、测试、脚本或根 `README.md`。
- 质量门与测试矩阵从实现阶段 P1 开始适用。

## 3. 质量门

后续任何代码变更在提交前必须全部通过：

```bash
gofmt -l .
go build ./...
go vet ./...
go test ./...
go test -race ./...
```

- `gofmt -l .` 必须无输出。
- 涉及 SQLite 后端的测试使用 `CGO_ENABLED=1`；本地工具链固定 `GOTOOLCHAIN=local`。
- 质量门在 flake devShell 中执行。

## 4. 代码约定

- 包名小写、短；导出符号必须有文档注释。
- 不使用 `panic` 表达可恢复错误；初始化期配置错误返回错误并由核心记录。
- 时间统一 UTC；存储与日志使用 `RFC3339Nano`。
- 日志字段固定、可检索；不记录 API key 与完整提示词，除非 `debug_prompts=true`。
- 测试只用标准库 `testing`；桩用手写实现，不引入第三方 mock。
- 新增行为先写文档，再写实现与测试。

## 5. 项目结构

目标实现文件与职责见 [`docs/architecture.md §5`](docs/architecture.md#5-目录与文件职责)。本文件不维护第二份文件表。

## 6. 文档索引

唯一文档索引是 [`docs/README.md`](docs/README.md)。本文件不维护第二份章节或文件索引。

## 7. 工作流

- 改行为先改文档；文档与代码冲突时以 kei 源码为准并同步文档。
- 配置键增删必须同时修改：[`docs/configuration.md §13.1`](docs/configuration.md#131-权威配置键表)、[`docs/architecture.md §6`](docs/architecture.md#6-与-kei-核心的契约对应) 的 `Metadata.Options`、示例配置、§13.4 校验规则。
- 表名、行键或行 JSON 结构变更必须同时修改 [`docs/architecture.md §4.4`](docs/architecture.md#44-storage-模式)、[`docs/vector-index.md §11.2`](docs/vector-index.md#112-数据结构)、[`docs/rag.md §10.5`](docs/rag.md#105-索引与更新)、[`docs/memory.md §9.1`](docs/memory.md#91-数据模型)、[`docs/testing.md §15.2`](docs/testing.md#152-单元测试矩阵)。
- 章号 1-16 全局唯一；新增或删除文档必须同步 [`docs/README.md`](docs/README.md) 的文档索引表。
- 交付状态只在 [`docs/roadmap.md §16.1`](docs/roadmap.md#161-现状) 更新。
- commit message 与 tag 使用英文。
