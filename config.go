package chatagent

import (
	"fmt"
	"math"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/RandomLemon/kei/pkg/bot"
)

// config 是插件全部配置的只读快照。
type config struct {
	GroupPolicy   string
	GroupList     []string
	PrivatePolicy string
	PrivateList   []string
	IgnoreBots    bool
	RespondToCmd  bool
	SelfIDs       []string
	TriggerWords  []string
	TriggerMinLen int
	SuperAdmins   []string

	MentionReplyProbability float64
	MentionMinInterval      time.Duration
	RandomEnabled           bool
	RandomProbability       float64
	RandomCooldown          time.Duration
	RandomMaxPerHour        int
	RandomMinParticipants   int
	RandomActivityWindow    time.Duration
	RandomQuietHours        string
	RandomTimezone          string
	location                *time.Location
	quietStartMin           int
	quietEndMin             int
	quietEnabled            bool

	BatchWindow         time.Duration
	BatchMaxWindow      time.Duration
	LimitsMaxConcurrent int
	ReplyMaxChars       int
	ReplyMentionSender  bool
	ReplyQuote          bool
	ReplyDedupe         bool

	HistoryMaxMessages        int
	HistoryMaxChars           int
	HistoryMaxSessions        int
	HistoryTTL                time.Duration
	HistoryUnconsolidatedTTL  time.Duration
	HistoryCleanupInterval    time.Duration
	ContextMaxMessages        int
	ContextMaxChars           int
	BotName                   string
	SystemPrompt              string
	LLMBaseURL                string
	LLMAPIKey                 string
	LLMModel                  string
	LLMTemperature            float64
	LLMMaxTokens              int
	LLMTimeout                time.Duration
	LLMMaxRetries             int
	LLMSkipToken              string
	LLMSkipEnabled            bool
	LLMExtraHeaders           map[string]string
	DebugPrompts              bool
	VisionEnabled             bool
	MemoryEnabled             bool
	RAGEnabled                bool
	EmbeddingBaseURL          string
	EmbeddingAPIKey           string
	EmbeddingModel            string
	EmbeddingDimensions       int
	EmbeddingTimeout          time.Duration
	EmbeddingMaxRetries       int
	EmbeddingExtraHeaders     map[string]string
	VectorMaxRecords          int
	MemoryConsolidateInterval time.Duration
	MemoryConsolidateSessions int
	MemoryConsolidateMessages int
	MemoryConsolidateTimeout  time.Duration
	MemoryExtractMax          int
	MemoryExtractMinMessages  int
	MemoryDedupeThreshold     float64
	MemoryMinScore            float64
	MemoryTopK                int
	MemoryContextMaxChars     int
	MemoryDecayHalfLife       time.Duration
	MemoryMinImportance       float64
	MemoryTTL                 time.Duration
	RAGContextMaxChars        int
	KnowledgeTopK             int
	KnowledgeMinScore         float64
	KnowledgeContextMaxChars  int
	KnowledgeChunkChars       int
	KnowledgeChunkOverlap     int
	KnowledgeMaxBytes         int
	KnowledgeAllowedTypes     []string
	KnowledgeURLAllowPrivate  bool
	JobsRunOnStart            bool
}

// defaultConfig 返回与 docs/configuration.md §13.1 一致的默认值。
func defaultConfig() *config {
	return &config{
		GroupPolicy:   "open",
		PrivatePolicy: "off",
		IgnoreBots:    true,
		TriggerMinLen: 2,

		MentionReplyProbability: 1.0,
		MentionMinInterval:      10 * time.Second,
		RandomEnabled:           true,
		RandomProbability:       0.12,
		RandomCooldown:          90 * time.Second,
		RandomMaxPerHour:        6,
		RandomMinParticipants:   2,
		RandomActivityWindow:    5 * time.Minute,
		RandomTimezone:          "Local",

		BatchWindow:         2500 * time.Millisecond,
		BatchMaxWindow:      8 * time.Second,
		LimitsMaxConcurrent: 2,
		ReplyMaxChars:       200,
		ReplyQuote:          true,
		ReplyDedupe:         true,

		HistoryMaxMessages:        30,
		HistoryMaxChars:           8000,
		HistoryMaxSessions:        512,
		HistoryTTL:                24 * time.Hour,
		HistoryUnconsolidatedTTL:  72 * time.Hour,
		HistoryCleanupInterval:    5 * time.Minute,
		ContextMaxMessages:        20,
		ContextMaxChars:           4000,
		BotName:                   "助手",
		LLMBaseURL:                "https://api.openai.com/v1",
		LLMTemperature:            0.8,
		LLMMaxTokens:              400,
		LLMTimeout:                30 * time.Second,
		LLMMaxRetries:             1,
		LLMSkipToken:              "[SKIP]",
		LLMSkipEnabled:            true,
		LLMExtraHeaders:           map[string]string{},
		MemoryEnabled:             true,
		RAGEnabled:                true,
		EmbeddingTimeout:          20 * time.Second,
		EmbeddingMaxRetries:       1,
		EmbeddingExtraHeaders:     map[string]string{},
		VectorMaxRecords:          10000,
		MemoryConsolidateInterval: 30 * time.Minute,
		MemoryConsolidateSessions: 16,
		MemoryConsolidateMessages: 50,
		MemoryConsolidateTimeout:  5 * time.Minute,
		MemoryExtractMax:          8,
		MemoryExtractMinMessages:  2,
		MemoryDedupeThreshold:     0.92,
		MemoryMinScore:            0.35,
		MemoryTopK:                5,
		MemoryContextMaxChars:     1200,
		MemoryDecayHalfLife:       720 * time.Hour,
		MemoryMinImportance:       0.15,
		RAGContextMaxChars:        2400,
		KnowledgeTopK:             3,
		KnowledgeMinScore:         0.30,
		KnowledgeContextMaxChars:  1200,
		KnowledgeChunkChars:       800,
		KnowledgeChunkOverlap:     100,
		KnowledgeMaxBytes:         1048576,
		KnowledgeAllowedTypes:     []string{"text/plain", "text/markdown", "text/html"},
	}
}

// loadConfig 读取并校验插件配置。返回的错误已是固定用户文案。
func loadConfig(c *bot.Config) (*config, error) {
	cfg := defaultConfig()
	var raw map[string]any
	if c != nil {
		raw = c.Raw()
	}
	if raw == nil {
		raw = map[string]any{}
	}

	cfg.GroupPolicy = readString(raw, "group_policy", cfg.GroupPolicy)
	cfg.GroupList = readListOrNil(raw, "group_list")
	cfg.PrivatePolicy = readString(raw, "private_policy", cfg.PrivatePolicy)
	cfg.PrivateList = readListOrNil(raw, "private_list")
	cfg.IgnoreBots = readBool(raw, "ignore_bots", cfg.IgnoreBots)
	cfg.RespondToCmd = readBool(raw, "respond_to_commands", cfg.RespondToCmd)
	cfg.SelfIDs = readListOrNil(raw, "self_ids")
	cfg.TriggerWords = readListOrNil(raw, "trigger_keywords")
	cfg.TriggerMinLen = readInt(raw, "trigger_min_chars", cfg.TriggerMinLen)
	cfg.SuperAdmins = readListOrNil(raw, "super_admins")

	cfg.MentionReplyProbability = readFloat(raw, "mention_reply_probability", cfg.MentionReplyProbability)
	cfg.MentionMinInterval = readDuration(raw, "mention_min_interval", cfg.MentionMinInterval)
	cfg.RandomEnabled = readBool(raw, "random_enabled", cfg.RandomEnabled)
	cfg.RandomProbability = readFloat(raw, "random_probability", cfg.RandomProbability)
	cfg.RandomCooldown = readDuration(raw, "random_cooldown", cfg.RandomCooldown)
	cfg.RandomMaxPerHour = readInt(raw, "random_max_per_hour", cfg.RandomMaxPerHour)
	cfg.RandomMinParticipants = readInt(raw, "random_min_participants", cfg.RandomMinParticipants)
	cfg.RandomActivityWindow = readDuration(raw, "random_activity_window", cfg.RandomActivityWindow)
	cfg.RandomQuietHours = readString(raw, "random_quiet_hours", cfg.RandomQuietHours)
	cfg.RandomTimezone = readString(raw, "random_timezone", cfg.RandomTimezone)

	cfg.BatchWindow = readDuration(raw, "batch_window", cfg.BatchWindow)
	cfg.BatchMaxWindow = readDuration(raw, "batch_max_window", cfg.BatchMaxWindow)
	cfg.LimitsMaxConcurrent = readInt(raw, "limits_max_concurrent", cfg.LimitsMaxConcurrent)
	cfg.ReplyMaxChars = readInt(raw, "reply_max_chars", cfg.ReplyMaxChars)
	cfg.ReplyMentionSender = readBool(raw, "reply_mention_sender", cfg.ReplyMentionSender)
	cfg.ReplyQuote = readBool(raw, "reply_quote", cfg.ReplyQuote)
	cfg.ReplyDedupe = readBool(raw, "reply_dedupe", cfg.ReplyDedupe)

	cfg.HistoryMaxMessages = readInt(raw, "history_max_messages", cfg.HistoryMaxMessages)
	cfg.HistoryMaxChars = readInt(raw, "history_max_chars", cfg.HistoryMaxChars)
	cfg.HistoryMaxSessions = readInt(raw, "history_max_sessions", cfg.HistoryMaxSessions)
	cfg.HistoryTTL = readDuration(raw, "history_ttl", cfg.HistoryTTL)
	cfg.HistoryUnconsolidatedTTL = readDuration(raw, "history_unconsolidated_ttl", cfg.HistoryUnconsolidatedTTL)
	cfg.HistoryCleanupInterval = readDuration(raw, "history_cleanup_interval", cfg.HistoryCleanupInterval)
	cfg.ContextMaxMessages = readInt(raw, "context_max_messages", cfg.ContextMaxMessages)
	cfg.ContextMaxChars = readInt(raw, "context_max_chars", cfg.ContextMaxChars)

	cfg.BotName = readString(raw, "bot_name", cfg.BotName)
	cfg.SystemPrompt = readString(raw, "system_prompt", cfg.SystemPrompt)
	cfg.LLMBaseURL = readString(raw, "llm_base_url", cfg.LLMBaseURL)
	cfg.LLMAPIKey = readString(raw, "llm_api_key", cfg.LLMAPIKey)
	cfg.LLMModel = readString(raw, "llm_model", cfg.LLMModel)
	cfg.LLMTemperature = readFloat(raw, "llm_temperature", cfg.LLMTemperature)
	cfg.LLMMaxTokens = readInt(raw, "llm_max_tokens", cfg.LLMMaxTokens)
	cfg.LLMTimeout = readDuration(raw, "llm_timeout", cfg.LLMTimeout)
	cfg.LLMMaxRetries = readInt(raw, "llm_max_retries", cfg.LLMMaxRetries)
	cfg.LLMSkipToken = readString(raw, "llm_skip_token", cfg.LLMSkipToken)
	cfg.LLMSkipEnabled = readBool(raw, "llm_skip_enabled", cfg.LLMSkipEnabled)
	cfg.LLMExtraHeaders = readHeadersOrNil(raw, "llm_extra_headers")
	cfg.DebugPrompts = readBool(raw, "debug_prompts", cfg.DebugPrompts)
	cfg.VisionEnabled = readBool(raw, "vision_enabled", cfg.VisionEnabled)

	cfg.MemoryEnabled = readBool(raw, "memory_enabled", cfg.MemoryEnabled)
	cfg.RAGEnabled = readBool(raw, "rag_enabled", cfg.RAGEnabled)
	cfg.EmbeddingBaseURL = readString(raw, "embedding_base_url", cfg.EmbeddingBaseURL)
	cfg.EmbeddingAPIKey = readString(raw, "embedding_api_key", cfg.EmbeddingAPIKey)
	cfg.EmbeddingModel = readString(raw, "embedding_model", cfg.EmbeddingModel)
	cfg.EmbeddingDimensions = readInt(raw, "embedding_dimensions", cfg.EmbeddingDimensions)
	cfg.EmbeddingTimeout = readDuration(raw, "embedding_timeout", cfg.EmbeddingTimeout)
	cfg.EmbeddingMaxRetries = readInt(raw, "embedding_max_retries", cfg.EmbeddingMaxRetries)
	cfg.EmbeddingExtraHeaders = readHeadersOrNil(raw, "embedding_extra_headers")
	cfg.VectorMaxRecords = readInt(raw, "vector_max_records", cfg.VectorMaxRecords)

	cfg.MemoryConsolidateInterval = readDuration(raw, "memory_consolidate_interval", cfg.MemoryConsolidateInterval)
	cfg.MemoryConsolidateSessions = readInt(raw, "memory_consolidate_max_sessions", cfg.MemoryConsolidateSessions)
	cfg.MemoryConsolidateMessages = readInt(raw, "memory_consolidate_max_messages", cfg.MemoryConsolidateMessages)
	cfg.MemoryConsolidateTimeout = readDuration(raw, "memory_consolidate_timeout", cfg.MemoryConsolidateTimeout)
	cfg.MemoryExtractMax = readInt(raw, "memory_extract_max", cfg.MemoryExtractMax)
	cfg.MemoryExtractMinMessages = readInt(raw, "memory_extract_min_messages", cfg.MemoryExtractMinMessages)
	cfg.MemoryDedupeThreshold = readFloat(raw, "memory_dedupe_threshold", cfg.MemoryDedupeThreshold)
	cfg.MemoryMinScore = readFloat(raw, "memory_min_score", cfg.MemoryMinScore)
	cfg.MemoryTopK = readInt(raw, "memory_top_k", cfg.MemoryTopK)
	cfg.MemoryContextMaxChars = readInt(raw, "memory_context_max_chars", cfg.MemoryContextMaxChars)
	cfg.MemoryDecayHalfLife = readDuration(raw, "memory_importance_decay_half_life", cfg.MemoryDecayHalfLife)
	cfg.MemoryMinImportance = readFloat(raw, "memory_min_importance", cfg.MemoryMinImportance)
	cfg.MemoryTTL = readDuration(raw, "memory_ttl", cfg.MemoryTTL)

	cfg.RAGContextMaxChars = readInt(raw, "rag_context_max_chars", cfg.RAGContextMaxChars)
	cfg.KnowledgeTopK = readInt(raw, "knowledge_top_k", cfg.KnowledgeTopK)
	cfg.KnowledgeMinScore = readFloat(raw, "knowledge_min_score", cfg.KnowledgeMinScore)
	cfg.KnowledgeContextMaxChars = readInt(raw, "knowledge_context_max_chars", cfg.KnowledgeContextMaxChars)
	cfg.KnowledgeChunkChars = readInt(raw, "knowledge_chunk_chars", cfg.KnowledgeChunkChars)
	cfg.KnowledgeChunkOverlap = readInt(raw, "knowledge_chunk_overlap", cfg.KnowledgeChunkOverlap)
	cfg.KnowledgeMaxBytes = readInt(raw, "knowledge_max_bytes", cfg.KnowledgeMaxBytes)
	cfg.KnowledgeAllowedTypes = readListOrNilDefault(raw, "knowledge_allowed_content_types", cfg.KnowledgeAllowedTypes)
	cfg.KnowledgeURLAllowPrivate = readBool(raw, "knowledge_url_allow_private", cfg.KnowledgeURLAllowPrivate)

	cfg.JobsRunOnStart = readBool(raw, "jobs_run_on_start", cfg.JobsRunOnStart)

	if err := cfg.validate(raw); err != nil {
		return nil, err
	}
	return cfg, nil
}

func (c *config) validate(raw map[string]any) error {
	if err := validateScalarTypes(raw); err != nil {
		return err
	}
	// 名单与准入
	if err := validateMode(raw, "group_policy", c.GroupPolicy); err != nil {
		return err
	}
	if err := validatePolicyListEntries(raw, "group_list"); err != nil {
		return err
	}
	if err := validateMode(raw, "private_policy", c.PrivatePolicy); err != nil {
		return err
	}
	if err := validatePolicyListEntries(raw, "private_list"); err != nil {
		return err
	}
	c.GroupList = normalizePlatformEntries(c.GroupList)
	c.PrivateList = normalizePlatformEntries(c.PrivateList)
	c.SuperAdmins = normalizePlatformEntries(c.SuperAdmins)
	c.SelfIDs = normalizePlatformEntries(c.SelfIDs)
	for _, key := range []string{"self_ids", "trigger_keywords"} {
		if err := validateListEntries(raw, key); err != nil {
			return err
		}
	}
	if reason := atLeast(c.TriggerMinLen, 0); reason != "" {
		return configError(raw, "trigger_min_chars", reason)
	}
	if err := validateListEntries(raw, "super_admins"); err != nil {
		return err
	}
	for _, entry := range c.SuperAdmins {
		if !validPlatformUser(entry) {
			return configError(raw, "super_admins", "必须是 platform:userID")
		}
	}

	// 参与与回复
	if err := validate01(raw, "mention_reply_probability", c.MentionReplyProbability); err != nil {
		return err
	}
	if reason := atLeastDuration(c.MentionMinInterval, 0); reason != "" {
		return configError(raw, "mention_min_interval", reason)
	}
	if reason := atLeast(c.RandomMaxPerHour, 0); reason != "" {
		return configError(raw, "random_max_per_hour", reason)
	}
	if err := validate01(raw, "random_probability", c.RandomProbability); err != nil {
		return err
	}
	if reason := atLeastDuration(c.RandomCooldown, 0); reason != "" {
		return configError(raw, "random_cooldown", reason)
	}
	if reason := atLeast(c.RandomMinParticipants, 0); reason != "" {
		return configError(raw, "random_min_participants", reason)
	}
	if reason := positiveDuration(c.RandomActivityWindow); reason != "" {
		return configError(raw, "random_activity_window", reason)
	}
	quietStart, quietEnd, quietEnabled, err := parseQuietHours(c.RandomQuietHours)
	if err != nil {
		return configError(raw, "random_quiet_hours", err.Error())
	}
	loc, err := time.LoadLocation(c.RandomTimezone)
	if err != nil {
		return configError(raw, "random_timezone", "不是合法时区")
	}
	c.location = loc
	c.quietStartMin, c.quietEndMin, c.quietEnabled = quietStart, quietEnd, quietEnabled

	if reason := positiveDuration(c.BatchWindow); reason != "" {
		return configError(raw, "batch_window", reason)
	}
	if c.BatchMaxWindow < c.BatchWindow {
		return configError(raw, "batch_max_window", "batch_max_window 必须 >= batch_window")
	}
	if reason := atLeast(c.LimitsMaxConcurrent, 1); reason != "" {
		return configError(raw, "limits_max_concurrent", reason)
	}
	if reason := atLeast(c.ReplyMaxChars, 1); reason != "" {
		return configError(raw, "reply_max_chars", reason)
	}

	// 历史与上下文
	if reason := atLeast(c.HistoryMaxMessages, 1); reason != "" {
		return configError(raw, "history_max_messages", reason)
	}
	if reason := atLeast(c.HistoryMaxChars, 1); reason != "" {
		return configError(raw, "history_max_chars", reason)
	}
	if reason := atLeast(c.HistoryMaxSessions, 1); reason != "" {
		return configError(raw, "history_max_sessions", reason)
	}
	if reason := atLeastDuration(c.HistoryTTL, 0); reason != "" {
		return configError(raw, "history_ttl", reason)
	}
	if reason := positiveDuration(c.HistoryUnconsolidatedTTL); reason != "" {
		return configError(raw, "history_unconsolidated_ttl", reason)
	}
	if reason := atLeastDuration(c.HistoryCleanupInterval, 0); reason != "" {
		return configError(raw, "history_cleanup_interval", reason)
	}
	if reason := atLeast(c.ContextMaxMessages, 1); reason != "" {
		return configError(raw, "context_max_messages", reason)
	}
	if reason := atLeast(c.ContextMaxChars, 1); reason != "" {
		return configError(raw, "context_max_chars", reason)
	}

	// Chat 模型与提示词
	if strings.TrimSpace(c.BotName) == "" {
		return configError(raw, "bot_name", "不能为空")
	}
	if strings.TrimSpace(c.LLMModel) == "" {
		return configError(raw, "llm_model", "llm_model 不能为空")
	}
	if err := validateURLString(raw, "llm_base_url", c.LLMBaseURL, false); err != nil {
		return err
	}
	if c.LLMTemperature < 0 {
		return configError(raw, "llm_temperature", "必须 >= 0")
	}
	if reason := atLeast(c.LLMMaxTokens, 1); reason != "" {
		return configError(raw, "llm_max_tokens", reason)
	}
	if reason := positiveDuration(c.LLMTimeout); reason != "" {
		return configError(raw, "llm_timeout", reason)
	}
	if reason := atLeast(c.LLMMaxRetries, 0); reason != "" {
		return configError(raw, "llm_max_retries", reason)
	}
	if strings.TrimSpace(c.LLMSkipToken) == "" {
		return configError(raw, "llm_skip_token", "不能为空")
	}
	if err := validateHeaders(raw, "llm_extra_headers"); err != nil {
		return err
	}

	// Embedding 与向量索引
	embeddingURL := c.EmbeddingBaseURL
	if embeddingURL == "" {
		embeddingURL = c.LLMBaseURL
	}
	if err := validateURLString(raw, "embedding_base_url", embeddingURL, true); err != nil {
		return err
	}
	if c.MemoryEnabled || c.RAGEnabled {
		if strings.TrimSpace(c.EmbeddingModel) == "" {
			return configError(raw, "embedding_model", "memory_enabled 或 rag_enabled 为 true 时必须配置 embedding_model")
		}
	}
	if reason := atLeast(c.EmbeddingDimensions, 0); reason != "" {
		return configError(raw, "embedding_dimensions", reason)
	}
	if reason := positiveDuration(c.EmbeddingTimeout); reason != "" {
		return configError(raw, "embedding_timeout", reason)
	}
	if reason := atLeast(c.EmbeddingMaxRetries, 0); reason != "" {
		return configError(raw, "embedding_max_retries", reason)
	}
	if err := validateHeaders(raw, "embedding_extra_headers"); err != nil {
		return err
	}
	if reason := atLeast(c.VectorMaxRecords, 1); reason != "" {
		return configError(raw, "vector_max_records", reason)
	}

	// 长期记忆
	if reason := atLeastDuration(c.MemoryConsolidateInterval, 0); reason != "" {
		return configError(raw, "memory_consolidate_interval", reason)
	}
	if reason := atLeast(c.MemoryConsolidateSessions, 1); reason != "" {
		return configError(raw, "memory_consolidate_max_sessions", reason)
	}
	if reason := atLeast(c.MemoryConsolidateMessages, 1); reason != "" {
		return configError(raw, "memory_consolidate_max_messages", reason)
	}
	if reason := positiveDuration(c.MemoryConsolidateTimeout); reason != "" {
		return configError(raw, "memory_consolidate_timeout", reason)
	}
	if reason := atLeast(c.MemoryExtractMax, 0); reason != "" {
		return configError(raw, "memory_extract_max", reason)
	}
	if reason := atLeast(c.MemoryExtractMinMessages, 1); reason != "" {
		return configError(raw, "memory_extract_min_messages", reason)
	}
	if err := validate01(raw, "memory_dedupe_threshold", c.MemoryDedupeThreshold); err != nil {
		return err
	}
	if err := validate01(raw, "memory_min_score", c.MemoryMinScore); err != nil {
		return err
	}
	if reason := atLeast(c.MemoryTopK, 0); reason != "" {
		return configError(raw, "memory_top_k", reason)
	}
	if reason := atLeast(c.MemoryContextMaxChars, 0); reason != "" {
		return configError(raw, "memory_context_max_chars", reason)
	}
	if reason := positiveDuration(c.MemoryDecayHalfLife); reason != "" {
		return configError(raw, "memory_importance_decay_half_life", reason)
	}
	if err := validate01(raw, "memory_min_importance", c.MemoryMinImportance); err != nil {
		return err
	}
	if reason := atLeastDuration(c.MemoryTTL, 0); reason != "" {
		return configError(raw, "memory_ttl", reason)
	}

	// 知识库 RAG
	if reason := atLeast(c.RAGContextMaxChars, 0); reason != "" {
		return configError(raw, "rag_context_max_chars", reason)
	}
	if reason := atLeast(c.KnowledgeTopK, 0); reason != "" {
		return configError(raw, "knowledge_top_k", reason)
	}
	if err := validate01(raw, "knowledge_min_score", c.KnowledgeMinScore); err != nil {
		return err
	}
	if reason := atLeast(c.KnowledgeContextMaxChars, 0); reason != "" {
		return configError(raw, "knowledge_context_max_chars", reason)
	}
	if reason := atLeast(c.KnowledgeChunkChars, 100); reason != "" {
		return configError(raw, "knowledge_chunk_chars", reason)
	}
	if c.KnowledgeChunkOverlap < 0 || c.KnowledgeChunkOverlap >= c.KnowledgeChunkChars {
		return configError(raw, "knowledge_chunk_overlap", "knowledge_chunk_overlap 必须 >= 0 且 < knowledge_chunk_chars")
	}
	if reason := atLeast(c.KnowledgeMaxBytes, 1); reason != "" {
		return configError(raw, "knowledge_max_bytes", reason)
	}
	if err := validateListEntries(raw, "knowledge_allowed_content_types"); err != nil {
		return err
	}
	types, err := normalizeContentTypes(c.KnowledgeAllowedTypes)
	if err != nil {
		return configError(raw, "knowledge_allowed_content_types", err.Error())
	}
	if len(types) == 0 {
		return configError(raw, "knowledge_allowed_content_types", "knowledge_allowed_content_types 不能为空")
	}
	c.KnowledgeAllowedTypes = types
	return nil
}

// effectiveEmbeddingURL 返回实际使用的 Embeddings base URL。
func (c *config) effectiveEmbeddingURL() string {
	if c.EmbeddingBaseURL != "" {
		return c.EmbeddingBaseURL
	}
	return c.LLMBaseURL
}

// effectiveEmbeddingKey 返回实际使用的 Embeddings API key。
func (c *config) effectiveEmbeddingKey() string {
	if c.EmbeddingAPIKey != "" {
		return c.EmbeddingAPIKey
	}
	return c.LLMAPIKey
}

// inQuietHours 判断给定时间是否落在静默时段内；区间包含开始、不包含结束。
func (c *config) inQuietHours(t time.Time) bool {
	if !c.quietEnabled {
		return false
	}
	local := t.In(c.location)
	m := local.Hour()*60 + local.Minute()
	if c.quietStartMin < c.quietEndMin {
		return m >= c.quietStartMin && m < c.quietEndMin
	}
	return m >= c.quietStartMin || m < c.quietEndMin
}

// formatTimestamp 使用配置时区格式化 {{now}} 占位符。
func (c *config) formatNow(t time.Time) string {
	return t.In(c.location).Format("2006-01-02 15:04:05 MST")
}

func configError(raw map[string]any, key, reason string) error {
	return fmt.Errorf("chat-agent: 配置错误 %s=%s: %s", key, rawDisplay(raw, key), reason)
}

func rawDisplay(raw map[string]any, key string) string {
	v, ok := raw[key]
	if !ok || v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprint(v)
}

func readString(raw map[string]any, key, def string) string {
	v, ok := raw[key]
	if !ok || v == nil {
		return def
	}
	switch t := v.(type) {
	case string:
		return strings.TrimSpace(t)
	default:
		return strings.TrimSpace(fmt.Sprint(t))
	}
}

func readBool(raw map[string]any, key string, def bool) bool {
	v, ok := raw[key]
	if !ok || v == nil {
		return def
	}
	switch t := v.(type) {
	case bool:
		return t
	case string:
		b, err := strconv.ParseBool(strings.TrimSpace(t))
		if err == nil {
			return b
		}
	}
	return def
}

func readInt(raw map[string]any, key string, def int) int {
	v, ok := raw[key]
	if !ok || v == nil {
		return def
	}
	switch t := v.(type) {
	case int:
		return t
	case int64:
		return int(t)
	case float64:
		return int(t)
	case string:
		n, err := strconv.Atoi(strings.TrimSpace(t))
		if err == nil {
			return n
		}
	}
	return def
}

func readFloat(raw map[string]any, key string, def float64) float64 {
	v, ok := raw[key]
	if !ok || v == nil {
		return def
	}
	switch t := v.(type) {
	case float64:
		return t
	case float32:
		return float64(t)
	case int:
		return float64(t)
	case int64:
		return float64(t)
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(t), 64)
		if err == nil {
			return f
		}
	}
	return def
}

func readDuration(raw map[string]any, key string, def time.Duration) time.Duration {
	v, ok := raw[key]
	if !ok || v == nil {
		return def
	}
	switch t := v.(type) {
	case time.Duration:
		return t
	case int:
		return time.Duration(t) * time.Second
	case int64:
		return time.Duration(t) * time.Second
	case float64:
		return time.Duration(t * float64(time.Second))
	case string:
		d, err := time.ParseDuration(strings.TrimSpace(t))
		if err == nil {
			return d
		}
	}
	return def
}

// readListOrNil 读取列表配置；非法值在 validate 阶段报告。
func readListOrNil(raw map[string]any, key string) []string {
	v, ok := raw[key]
	if !ok || v == nil {
		return nil
	}
	list, err := parseStringList(v)
	if err != nil {
		return nil
	}
	return list
}

func readListOrNilDefault(raw map[string]any, key string, def []string) []string {
	if _, ok := raw[key]; !ok {
		return append([]string(nil), def...)
	}
	return readListOrNil(raw, key)
}

// parseStringList 接受标量、逗号分隔字符串或 YAML 列表，去空白、去重保序。
func parseStringList(v any) ([]string, error) {
	var items []string
	switch t := v.(type) {
	case nil:
		return nil, nil
	case string:
		if strings.TrimSpace(t) == "" {
			return nil, nil
		}
		parts := strings.Split(t, ",")
		for _, p := range parts {
			p = strings.TrimSpace(p)
			if p == "" {
				return nil, fmt.Errorf("条目不能为空")
			}
			items = append(items, p)
		}
	case []string:
		for _, s := range t {
			s = strings.TrimSpace(s)
			if s == "" {
				return nil, fmt.Errorf("条目不能为空")
			}
			items = append(items, s)
		}
	case []any:
		for _, item := range t {
			s := strings.TrimSpace(fmt.Sprint(item))
			if s == "" {
				return nil, fmt.Errorf("条目不能为空")
			}
			items = append(items, s)
		}
	case map[string]any, map[string]string:
		return nil, fmt.Errorf("必须是非空对象")
	default:
		s := strings.TrimSpace(fmt.Sprint(t))
		if s == "" {
			return nil, fmt.Errorf("条目不能为空")
		}
		items = append(items, s)
	}
	return dedupeStrings(items), nil
}

func validateListEntries(raw map[string]any, key string) error {
	v, ok := raw[key]
	if !ok || v == nil {
		return nil
	}
	_, err := parseStringList(v)
	if err != nil {
		return configError(raw, key, err.Error())
	}
	return nil
}

func validatePolicyListEntries(raw map[string]any, key string) error {
	if err := validateListEntries(raw, key); err != nil {
		return err
	}
	list, _ := parseStringList(raw[key])
	for _, entry := range list {
		platform, id, ok := strings.Cut(entry, ":")
		if !ok {
			continue
		}
		if strings.TrimSpace(platform) == "" || strings.TrimSpace(id) == "" {
			return configError(raw, key, "条目必须是 platform:id")
		}
	}
	return nil
}

func normalizePlatformEntry(s string) string {
	platform, id, ok := strings.Cut(strings.TrimSpace(s), ":")
	if !ok {
		return strings.TrimSpace(s)
	}
	return strings.TrimSpace(platform) + ":" + strings.TrimSpace(id)
}

func normalizePlatformEntries(in []string) []string {
	if len(in) == 0 {
		return in
	}
	out := make([]string, len(in))
	for i, s := range in {
		out[i] = normalizePlatformEntry(s)
	}
	return out
}

func validateScalarTypes(raw map[string]any) error {
	boolKeys := []string{
		"ignore_bots", "respond_to_commands", "random_enabled", "reply_mention_sender",
		"reply_quote", "reply_dedupe", "debug_prompts", "vision_enabled",
		"memory_enabled", "rag_enabled", "knowledge_url_allow_private", "jobs_run_on_start",
	}
	intKeys := []string{
		"trigger_min_chars", "random_max_per_hour", "random_min_participants",
		"limits_max_concurrent", "reply_max_chars", "history_max_messages", "history_max_chars",
		"history_max_sessions", "context_max_messages", "context_max_chars", "llm_max_tokens",
		"llm_max_retries", "embedding_dimensions", "embedding_max_retries", "vector_max_records",
		"memory_consolidate_max_sessions", "memory_consolidate_max_messages", "memory_extract_max",
		"memory_extract_min_messages", "memory_top_k", "memory_context_max_chars",
		"rag_context_max_chars", "knowledge_top_k", "knowledge_context_max_chars",
		"knowledge_chunk_chars", "knowledge_chunk_overlap", "knowledge_max_bytes",
	}
	floatKeys := []string{
		"mention_reply_probability", "random_probability", "llm_temperature",
		"memory_dedupe_threshold", "memory_min_score", "memory_min_importance", "knowledge_min_score",
	}
	durationKeys := []string{
		"mention_min_interval", "random_cooldown", "random_activity_window", "batch_window",
		"batch_max_window", "history_ttl", "history_unconsolidated_ttl", "history_cleanup_interval",
		"llm_timeout", "embedding_timeout", "memory_consolidate_interval",
		"memory_consolidate_timeout", "memory_importance_decay_half_life", "memory_ttl",
	}
	stringKeys := []string{
		"group_policy", "private_policy", "bot_name", "system_prompt", "llm_base_url",
		"llm_api_key", "llm_model", "llm_skip_token", "embedding_base_url", "embedding_api_key",
		"embedding_model", "random_quiet_hours", "random_timezone",
	}
	for _, key := range boolKeys {
		if v, ok := raw[key]; ok && v != nil && !validBoolValue(v) {
			return configError(raw, key, "必须是布尔值")
		}
	}
	for _, key := range intKeys {
		if v, ok := raw[key]; ok && v != nil && !validIntValue(v) {
			return configError(raw, key, "必须是整数")
		}
	}
	for _, key := range floatKeys {
		if v, ok := raw[key]; ok && v != nil {
			if !validNumberValue(v) {
				return configError(raw, key, "必须是数字")
			}
		}
	}
	for _, key := range durationKeys {
		if v, ok := raw[key]; ok && v != nil && !validDurationValue(v) {
			return configError(raw, key, "必须是时长")
		}
	}
	for _, key := range stringKeys {
		if v, ok := raw[key]; ok && v != nil {
			if _, ok := v.(string); !ok {
				return configError(raw, key, "必须是字符串")
			}
		}
	}
	return nil
}

func validBoolValue(v any) bool {
	switch t := v.(type) {
	case bool:
		return true
	case string:
		_, err := strconv.ParseBool(strings.TrimSpace(t))
		return err == nil
	default:
		return false
	}
}

func validIntValue(v any) bool {
	switch t := v.(type) {
	case int:
		return true
	case int64:
		return int64(int(t)) == t
	case float64:
		return !math.IsNaN(t) && !math.IsInf(t, 0) && math.Trunc(t) == t &&
			t >= float64(-int(^uint(0)>>1)-1) && t <= float64(int(^uint(0)>>1))
	case string:
		_, err := strconv.Atoi(strings.TrimSpace(t))
		return err == nil
	default:
		return false
	}
}

func validNumberValue(v any) bool {
	switch t := v.(type) {
	case int:
		return true
	case int64:
		return true
	case float32:
		return !math.IsNaN(float64(t)) && !math.IsInf(float64(t), 0)
	case float64:
		return !math.IsNaN(t) && !math.IsInf(t, 0)
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(t), 64)
		return err == nil && !math.IsNaN(f) && !math.IsInf(f, 0)
	default:
		return false
	}
}

func validDurationValue(v any) bool {
	switch t := v.(type) {
	case time.Duration:
		return true
	case int, int64:
		return true
	case float64:
		return !math.IsNaN(t) && !math.IsInf(t, 0)
	case string:
		_, err := time.ParseDuration(strings.TrimSpace(t))
		return err == nil
	default:
		return false
	}
}

func dedupeStrings(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}

func readHeadersOrNil(raw map[string]any, key string) map[string]string {
	v, ok := raw[key]
	if !ok || v == nil {
		return map[string]string{}
	}
	headers, err := parseHeaders(v)
	if err != nil {
		return map[string]string{}
	}
	return headers
}

func parseHeaders(v any) (map[string]string, error) {
	out := map[string]string{}
	switch t := v.(type) {
	case map[string]string:
		for k, val := range t {
			if strings.TrimSpace(k) == "" {
				return nil, fmt.Errorf("必须是字符串键值映射")
			}
			out[k] = val
		}
	case map[string]any:
		for k, val := range t {
			if strings.TrimSpace(k) == "" {
				return nil, fmt.Errorf("必须是字符串键值映射")
			}
			s, ok := val.(string)
			if !ok {
				return nil, fmt.Errorf("必须是字符串键值映射")
			}
			out[k] = s
		}
	case nil:
		return out, nil
	default:
		return nil, fmt.Errorf("必须是非空对象")
	}
	return out, nil
}

func validateHeaders(raw map[string]any, key string) error {
	v, ok := raw[key]
	if !ok || v == nil {
		return nil
	}
	headers, err := parseHeaders(v)
	if err != nil {
		return configError(raw, key, err.Error())
	}
	for k := range headers {
		switch strings.ToLower(strings.TrimSpace(k)) {
		case "authorization", "content-type", "content-length", "host":
			return configError(raw, key, "不得覆盖协议请求头")
		}
	}
	return nil
}

func validateMode(raw map[string]any, key, mode string) error {
	switch mode {
	case "off", "open", "whitelist", "blacklist":
		return nil
	default:
		return configError(raw, key, "必须是 off|open|whitelist|blacklist 之一")
	}
}

func validate01(raw map[string]any, key string, v float64) error {
	if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 || v > 1 {
		return configError(raw, key, "必须是 0..1 之间的小数")
	}
	return nil
}

func atLeast(v, min int) string {
	if v < min {
		return fmt.Sprintf("必须 >= %d", min)
	}
	return ""
}

func atLeastDuration(v, min time.Duration) string {
	if v < min {
		return fmt.Sprintf("必须 >= %d", min)
	}
	return ""
}

func positiveDuration(v time.Duration) string {
	if v <= 0 {
		return "必须 > 0"
	}
	return ""
}

func validPlatformUser(s string) bool {
	platform, userID, ok := strings.Cut(normalizePlatformEntry(s), ":")
	return ok && platform != "" && userID != ""
}

func validateURLString(raw map[string]any, key, value string, allowEmpty bool) error {
	value = strings.TrimSpace(value)
	if value == "" {
		if allowEmpty {
			return nil
		}
		return configError(raw, key, "必须是带 host 的 http(s) URL")
	}
	u, err := url.Parse(value)
	if err != nil {
		return configError(raw, key, "必须是带 host 的 http(s) URL")
	}
	scheme := strings.ToLower(u.Scheme)
	if (scheme != "http" && scheme != "https") || u.Hostname() == "" {
		return configError(raw, key, "必须是带 host 的 http(s) URL")
	}
	if u.User != nil {
		return configError(raw, key, "不得包含 userinfo")
	}
	return nil
}

func parseQuietHours(s string) (start, end int, enabled bool, err error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, 0, false, nil
	}
	if len(s) != len("15:04-15:04") || s[2] != ':' || s[5] != '-' || s[8] != ':' {
		return 0, 0, false, fmt.Errorf("必须是 HH:MM-HH:MM 格式")
	}
	parseClock := func(part string) (int, bool) {
		h, e1 := strconv.Atoi(part[0:2])
		m, e2 := strconv.Atoi(part[3:5])
		if e1 != nil || e2 != nil || h < 0 || h > 23 || m < 0 || m > 59 {
			return 0, false
		}
		return h*60 + m, true
	}
	a, ok1 := parseClock(s[0:5])
	b, ok2 := parseClock(s[6:11])
	if !ok1 || !ok2 {
		return 0, 0, false, fmt.Errorf("必须是 HH:MM-HH:MM 格式")
	}
	return a, b, a != b, nil
}

func normalizeContentTypes(in []string) ([]string, error) {
	out := make([]string, 0, len(in))
	for _, s := range in {
		s = strings.TrimSpace(strings.ToLower(s))
		if i := strings.IndexByte(s, ';'); i >= 0 {
			s = strings.TrimSpace(s[:i])
		}
		if s == "" {
			return nil, fmt.Errorf("条目不能为空")
		}
		out = append(out, s)
	}
	return dedupeStrings(out), nil
}
