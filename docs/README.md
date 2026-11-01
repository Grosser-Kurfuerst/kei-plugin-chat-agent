# chat-agent 文档

覆盖：`chat-agent` 插件的目标设计、配置、命令、算法、测试与交付状态。不覆盖：kei 宿主核心实现与平台适配器内部细节，这些以 kei 源码为准。

本仓库当前只有文档，没有代码。实现状态只看 [`roadmap.md §16.1`](roadmap.md#161-现状)。文档是后续实现的验收基线。

## 文档索引

| 文件 | 章号 | 内容 |
|---|---|---|
| `README.md` | — | 索引、阅读顺序、文档约定 |
| [`architecture.md`](architecture.md) | 1-6 | 定位与边界（含可选图片识别）、架构与数据流、生命周期与并发、状态与持久化、Storage 与表存储、目录与文件职责、与 kei 契约 |
| [`participation.md`](participation.md) | 7 | 事件过滤（含图片准入）、寻址、随机参与、批处理、决策伪代码、reason 词表、名单策略 |
| [`conversation.md`](conversation.md) | 8 | 会话状态、历史记录、图片占位与多模态上下文、缓存清理、重启语义 |
| [`memory.md`](memory.md) | 9 | 长期记忆数据模型、抽取、合并、整理、检索、注入、图片识别文本记忆、管理 |
| [`rag.md`](rag.md) | 10 | 知识库导入、抓取、分块、索引、检索、注入、双库边界 |
| [`vector-index.md`](vector-index.md) | 11 | TableStorage 表、写入协议、top-k、重建、容量、故障 |
| [`llm.md`](llm.md) | 12 | Chat/Embedding 协议（Chat 视觉内容块、Embedding 文本）、超时重试、限额、安全、兼容服务 |
| [`configuration.md`](configuration.md) | 13 | 权威配置键表（含图片开关）、示例、环境变量、校验规则 |
| [`admin.md`](admin.md) | 14 | 超级管理员与核心管理员、命令面、输出、持久化、审计 |
| [`testing.md`](testing.md) | 15 | 质量门、单测矩阵（含图片）、注入缝、mock 适配器端到端、竞态与关闭、文档校验 |
| [`roadmap.md`](roadmap.md) | 16 | 交付状态、P1-P8、已知缺口、假设与兜底 |

## 阅读顺序

1. [`architecture.md`](architecture.md)：先建立边界、数据流、生命周期与持久化模型。
2. [`participation.md`](participation.md)：理解一条消息如何被过滤、寻址、随机选中或批处理。
3. [`conversation.md`](conversation.md)：理解会话状态、历史记录、上下文渲染与清理。
4. [`memory.md`](memory.md)：理解长期记忆的抽取、合并、衰减、检索与注入。
5. [`rag.md`](rag.md)：理解知识库的导入、抓取、分块、索引与检索。
6. [`vector-index.md`](vector-index.md)：理解两个子系统共用的 `vector` 表、写入协议与检索算法。
7. [`llm.md`](llm.md)：理解 Chat 与 Embedding 的请求、响应、重试与错误语义。
8. [`configuration.md`](configuration.md)：核对全部配置键、默认值、示例与校验。
9. [`admin.md`](admin.md)：核对两套管理员身份、命令语法、输出与恢复路径。
10. [`testing.md`](testing.md)：核对质量门、测试矩阵、端到端场景与完成定义。
11. [`roadmap.md`](roadmap.md)：核对当前状态、实现阶段、已知缺口与假设。

## 文档约定

- 正文中文；标识符、类型名、配置键、命令、错误文案保持英文或原样。
- 每篇 `docs/*.md` 开头写“覆盖… 不覆盖…”，结尾写“相关文档”。
- 章号写成 `## 7.`，节号写成 `### 7.8`；章号 1-16 连续且全局唯一。
- 跨文档引用统一写成 `文件.md#锚点`，指向存在的文件与标题。
- [`configuration.md §13.1`](configuration.md#131-权威配置键表) 是配置键唯一权威；其它文档引用配置键时只引用，不重新定义默认值。
- [`roadmap.md §16.1`](roadmap.md#161-现状) 是交付状态唯一口径；其它文档不写“已实现/未实现”。
- 固定字面量只在一处定义，其它位置逐字引用：默认系统提示词、记忆抽取提示词、决策 reason 词表、存储键与表行形状、命令输出、配置错误文案。
- 不写把设计决定留给实现者的表述；边界只写“不支持 X”。
- 新增或删除文档必须同步“文档索引”表。

## 相关文档

- 全部章节目录：[`README.md` 的“文档索引”表](README.md#文档索引)。
- 配置键：[`configuration.md §13.1`](configuration.md#131-权威配置键表)。
- 命令面：[`admin.md §14.2-14.3`](admin.md#142-命令语法)。
- 交付状态：[`roadmap.md §16.1`](roadmap.md#161-现状)。
