package chatagent

import "github.com/RandomLemon/kei/pkg/bot"

const (
	// pluginName 是插件在核心中的唯一名称。
	pluginName = "chat-agent"
	// pluginVersion 是插件版本。
	pluginVersion = "v0.1.0"
)

// metadataOptions 是 Metadata.Options 的权威清单，必须与
// docs/configuration.md §13.1 的 82 个顶层键逐字一致。
var metadataOptions = []string{
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

// init 把插件注册进核心的编译期插件表；是否装配由宿主配置决定。
func init() {
	bot.RegisterPlugin(&Plugin{})
}

// Metadata 返回插件元信息。
func (p *Plugin) Metadata() bot.Metadata {
	return bot.Metadata{
		Name:        pluginName,
		Version:     pluginVersion,
		Author:      "RandomLemon",
		Description: "带长期记忆与知识库 RAG 的聊天代理",
		Permissions: []bot.Permission{
			bot.PermNetwork,
			bot.PermStorage,
			bot.PermSendMessage,
		},
		Options: optionsCopy(),
	}
}

// optionsCopy 返回 Metadata.Options 的副本，避免调用方修改包级清单。
func optionsCopy() []string {
	return append([]string(nil), metadataOptions...)
}
