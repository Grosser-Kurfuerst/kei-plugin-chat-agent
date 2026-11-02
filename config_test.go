package chatagent

import (
	"strings"
	"testing"
	"time"

	"github.com/RandomLemon/kei/pkg/bot"
)

func mustConfig(t *testing.T, raw map[string]any) *config {
	t.Helper()
	if raw == nil {
		raw = map[string]any{}
	}
	if _, ok := raw["llm_model"]; !ok {
		raw["llm_model"] = "test-model"
	}
	if _, ok := raw["embedding_model"]; !ok {
		raw["embedding_model"] = "test-embed"
	}
	cfg, err := loadConfig(bot.NewConfig(raw))
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	return cfg
}

func TestConfigDefaults(t *testing.T) {
	cfg := mustConfig(t, nil)
	checks := []struct {
		name string
		got  any
		want any
	}{
		{"group_policy", cfg.GroupPolicy, "open"},
		{"private_policy", cfg.PrivatePolicy, "off"},
		{"ignore_bots", cfg.IgnoreBots, true},
		{"respond_to_commands", cfg.RespondToCmd, false},
		{"trigger_min_chars", cfg.TriggerMinLen, 2},
		{"mention_reply_probability", cfg.MentionReplyProbability, 1.0},
		{"mention_min_interval", cfg.MentionMinInterval, 10 * time.Second},
		{"random_enabled", cfg.RandomEnabled, true},
		{"random_probability", cfg.RandomProbability, 0.12},
		{"random_cooldown", cfg.RandomCooldown, 90 * time.Second},
		{"random_max_per_hour", cfg.RandomMaxPerHour, 6},
		{"random_min_participants", cfg.RandomMinParticipants, 2},
		{"random_activity_window", cfg.RandomActivityWindow, 5 * time.Minute},
		{"random_timezone", cfg.RandomTimezone, "Local"},
		{"batch_window", cfg.BatchWindow, 2500 * time.Millisecond},
		{"batch_max_window", cfg.BatchMaxWindow, 8 * time.Second},
		{"limits_max_concurrent", cfg.LimitsMaxConcurrent, 2},
		{"reply_max_chars", cfg.ReplyMaxChars, 200},
		{"reply_quote", cfg.ReplyQuote, true},
		{"reply_dedupe", cfg.ReplyDedupe, true},
		{"history_max_messages", cfg.HistoryMaxMessages, 30},
		{"history_max_chars", cfg.HistoryMaxChars, 8000},
		{"history_max_sessions", cfg.HistoryMaxSessions, 512},
		{"history_ttl", cfg.HistoryTTL, 24 * time.Hour},
		{"history_unconsolidated_ttl", cfg.HistoryUnconsolidatedTTL, 72 * time.Hour},
		{"history_cleanup_interval", cfg.HistoryCleanupInterval, 5 * time.Minute},
		{"context_max_messages", cfg.ContextMaxMessages, 20},
		{"context_max_chars", cfg.ContextMaxChars, 4000},
		{"bot_name", cfg.BotName, "助手"},
		{"llm_base_url", cfg.LLMBaseURL, "https://api.openai.com/v1"},
		{"llm_temperature", cfg.LLMTemperature, 0.8},
		{"llm_max_tokens", cfg.LLMMaxTokens, 400},
		{"llm_timeout", cfg.LLMTimeout, 30 * time.Second},
		{"llm_max_retries", cfg.LLMMaxRetries, 1},
		{"llm_skip_token", cfg.LLMSkipToken, "[SKIP]"},
		{"llm_skip_enabled", cfg.LLMSkipEnabled, true},
		{"vision_enabled", cfg.VisionEnabled, false},
		{"memory_enabled", cfg.MemoryEnabled, true},
		{"rag_enabled", cfg.RAGEnabled, true},
		{"embedding_dimensions", cfg.EmbeddingDimensions, 0},
		{"embedding_timeout", cfg.EmbeddingTimeout, 20 * time.Second},
		{"embedding_max_retries", cfg.EmbeddingMaxRetries, 1},
		{"vector_max_records", cfg.VectorMaxRecords, 10000},
		{"memory_consolidate_interval", cfg.MemoryConsolidateInterval, 30 * time.Minute},
		{"memory_consolidate_max_sessions", cfg.MemoryConsolidateSessions, 16},
		{"memory_consolidate_max_messages", cfg.MemoryConsolidateMessages, 50},
		{"memory_consolidate_timeout", cfg.MemoryConsolidateTimeout, 5 * time.Minute},
		{"memory_extract_max", cfg.MemoryExtractMax, 8},
		{"memory_extract_min_messages", cfg.MemoryExtractMinMessages, 2},
		{"memory_dedupe_threshold", cfg.MemoryDedupeThreshold, 0.92},
		{"memory_min_score", cfg.MemoryMinScore, 0.35},
		{"memory_top_k", cfg.MemoryTopK, 5},
		{"memory_context_max_chars", cfg.MemoryContextMaxChars, 1200},
		{"memory_importance_decay_half_life", cfg.MemoryDecayHalfLife, 720 * time.Hour},
		{"memory_min_importance", cfg.MemoryMinImportance, 0.15},
		{"memory_ttl", cfg.MemoryTTL, time.Duration(0)},
		{"rag_context_max_chars", cfg.RAGContextMaxChars, 2400},
		{"knowledge_top_k", cfg.KnowledgeTopK, 3},
		{"knowledge_min_score", cfg.KnowledgeMinScore, 0.30},
		{"knowledge_context_max_chars", cfg.KnowledgeContextMaxChars, 1200},
		{"knowledge_chunk_chars", cfg.KnowledgeChunkChars, 800},
		{"knowledge_chunk_overlap", cfg.KnowledgeChunkOverlap, 100},
		{"knowledge_max_bytes", cfg.KnowledgeMaxBytes, 1048576},
		{"knowledge_url_allow_private", cfg.KnowledgeURLAllowPrivate, false},
		{"jobs_run_on_start", cfg.JobsRunOnStart, false},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %v, want %v", c.name, c.got, c.want)
		}
	}
	if got := strings.Join(cfg.KnowledgeAllowedTypes, ","); got != "text/plain,text/markdown,text/html" {
		t.Fatalf("allowed content types = %q", got)
	}
}

func TestConfigValidationMessages(t *testing.T) {
	cases := []struct {
		name  string
		raw   map[string]any
		key   string
		value string
		why   string
	}{
		{"mode", map[string]any{"group_policy": "nope"}, "group_policy", "nope", "必须是 off|open|whitelist|blacklist 之一"},
		{"float low", map[string]any{"random_probability": -0.1}, "random_probability", "-0.1", "必须是 0..1 之间的小数"},
		{"float high", map[string]any{"memory_min_score": 1.1}, "memory_min_score", "1.1", "必须是 0..1 之间的小数"},
		{"int low", map[string]any{"reply_max_chars": 0}, "reply_max_chars", "0", "必须 >= 1"},
		{"duration low", map[string]any{"random_activity_window": "0s"}, "random_activity_window", "0s", "必须 > 0"},
		{"negative duration", map[string]any{"history_ttl": "-1s"}, "history_ttl", "-1s", "必须 >= 0"},
		{"url missing host", map[string]any{"llm_base_url": "http://"}, "llm_base_url", "http://", "必须是带 host 的 http(s) URL"},
		{"url scheme", map[string]any{"llm_base_url": "ftp://example.com"}, "llm_base_url", "ftp://example.com", "必须是带 host 的 http(s) URL"},
		{"url userinfo", map[string]any{"llm_base_url": "https://u:p@example.com"}, "llm_base_url", "https://u:p@example.com", "不得包含 userinfo"},
		{"timezone", map[string]any{"random_timezone": "Mars/Base"}, "random_timezone", "Mars/Base", "不是合法时区"},
		{"quiet format", map[string]any{"random_quiet_hours": "25:00-01:00"}, "random_quiet_hours", "25:00-01:00", "必须是 HH:MM-HH:MM 格式"},
		{"admin", map[string]any{"super_admins": []string{"nocolon"}}, "super_admins", "[nocolon]", "必须是 platform:userID"},
		{"admin empty side", map[string]any{"super_admins": []string{"telegram:"}}, "super_admins", "[telegram:]", "必须是 platform:userID"},
		{"headers", map[string]any{"llm_extra_headers": map[string]any{"Authorization": "x"}}, "llm_extra_headers", "map[Authorization:x]", "不得覆盖协议请求头"},
		{"headers type", map[string]any{"llm_extra_headers": "x"}, "llm_extra_headers", "x", "必须是非空对象"},
		{"embedding required", map[string]any{"memory_enabled": true, "rag_enabled": true, "embedding_model": ""}, "embedding_model", "", "memory_enabled 或 rag_enabled 为 true 时必须配置 embedding_model"},
		{"batch relation", map[string]any{"batch_window": "10s", "batch_max_window": "5s"}, "batch_max_window", "5s", "batch_max_window 必须 >= batch_window"},
		{"chunk overlap", map[string]any{"knowledge_chunk_chars": 100, "knowledge_chunk_overlap": 100}, "knowledge_chunk_overlap", "100", "knowledge_chunk_overlap 必须 >= 0 且 < knowledge_chunk_chars"},
		{"chunk min", map[string]any{"knowledge_chunk_chars": 99}, "knowledge_chunk_chars", "99", "必须 >= 100"},
		{"content types empty", map[string]any{"knowledge_allowed_content_types": []string{}}, "knowledge_allowed_content_types", "[]", "knowledge_allowed_content_types 不能为空"},
		{"list empty entry", map[string]any{"group_list": []string{"a", ""}}, "group_list", "[a ]", "条目不能为空"},
		{"bot name", map[string]any{"bot_name": ""}, "bot_name", "", "不能为空"},
		{"skip token", map[string]any{"llm_skip_token": " "}, "llm_skip_token", " ", "不能为空"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := loadConfig(bot.NewConfig(withModels(tc.raw)))
			if err == nil {
				t.Fatal("want error")
			}
			msg := err.Error()
			want := "chat-agent: 配置错误 " + tc.key + "=" + tc.value + ": " + tc.why
			if msg != want {
				t.Fatalf("error = %q, want %q", msg, want)
			}
		})
	}
}

func withModels(raw map[string]any) map[string]any {
	out := map[string]any{"llm_model": "m", "embedding_model": "e"}
	for k, v := range raw {
		out[k] = v
	}
	return out
}

func TestConfigFloatBoundariesAndHeaders(t *testing.T) {
	for _, v := range []float64{0, 1} {
		cfg := mustConfig(t, map[string]any{
			"random_probability":      v,
			"memory_dedupe_threshold": v,
			"knowledge_min_score":     v,
		})
		if cfg.RandomProbability != v || cfg.MemoryDedupeThreshold != v || cfg.KnowledgeMinScore != v {
			t.Fatalf("boundary %v not accepted", v)
		}
	}
	cfg := mustConfig(t, map[string]any{
		"llm_extra_headers":       map[string]any{"X-Org": "kei"},
		"embedding_extra_headers": map[string]any{"X-Embed": "1"},
	})
	if cfg.LLMExtraHeaders["X-Org"] != "kei" || cfg.EmbeddingExtraHeaders["X-Embed"] != "1" {
		t.Fatalf("headers=%v %v", cfg.LLMExtraHeaders, cfg.EmbeddingExtraHeaders)
	}
	if _, err := loadConfig(bot.NewConfig(withModels(map[string]any{
		"llm_extra_headers": map[string]any{"": "x"},
	}))); err == nil {
		t.Fatal("empty header name accepted")
	}
}

func TestConfigLLMModelRequired(t *testing.T) {
	_, err := loadConfig(bot.NewConfig(map[string]any{"embedding_model": "e"}))
	if err == nil || err.Error() != "chat-agent: 配置错误 llm_model=: llm_model 不能为空" {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestConfigListsAndNormalization(t *testing.T) {
	cfg := mustConfig(t, map[string]any{
		"group_list":                      "a, b,a",
		"private_list":                    []string{" x ", "y"},
		"trigger_keywords":                "k1,k2",
		"knowledge_allowed_content_types": []string{"Text/HTML; charset=utf-8", "text/plain"},
	})
	if got := strings.Join(cfg.GroupList, ","); got != "a,b" {
		t.Fatalf("group list = %q", got)
	}
	if got := strings.Join(cfg.PrivateList, ","); got != "x,y" {
		t.Fatalf("private list = %q", got)
	}
	if got := strings.Join(cfg.KnowledgeAllowedTypes, ","); got != "text/html,text/plain" {
		t.Fatalf("types = %q", got)
	}
}

func TestConfigQuietHoursBoundaries(t *testing.T) {
	cfg := mustConfig(t, map[string]any{"random_quiet_hours": "23:00-07:00", "random_timezone": "UTC"})
	loc := cfg.location
	at := func(h, m int) time.Time {
		return time.Date(2026, 1, 1, h, m, 0, 0, loc)
	}
	if !cfg.inQuietHours(at(23, 30)) || !cfg.inQuietHours(at(6, 59)) {
		t.Fatal("cross-midnight quiet hours not applied")
	}
	if cfg.inQuietHours(at(7, 0)) || cfg.inQuietHours(at(12, 0)) {
		t.Fatal("quiet hours leaked outside range")
	}
	disabled := mustConfig(t, map[string]any{"random_quiet_hours": "00:00-00:00"})
	if disabled.inQuietHours(at(0, 0)) {
		t.Fatal("start==end must disable quiet hours")
	}
}

func TestConfigLLMAndEmbeddingFallbacks(t *testing.T) {
	cfg := mustConfig(t, map[string]any{
		"llm_base_url":       "http://127.0.0.1:1234/v1",
		"llm_api_key":        "k",
		"embedding_base_url": "",
		"embedding_api_key":  "",
	})
	if cfg.effectiveEmbeddingURL() != "http://127.0.0.1:1234/v1" {
		t.Fatalf("embedding url = %q", cfg.effectiveEmbeddingURL())
	}
	if cfg.effectiveEmbeddingKey() != "k" {
		t.Fatalf("embedding key = %q", cfg.effectiveEmbeddingKey())
	}
}

func TestMetadataOptions(t *testing.T) {
	if len(metadataOptions) != 82 {
		t.Fatalf("options = %d, want 82", len(metadataOptions))
	}
	seen := map[string]bool{}
	for _, k := range metadataOptions {
		if seen[k] {
			t.Fatalf("duplicate option %q", k)
		}
		seen[k] = true
	}
	if !seen["vision_enabled"] || !seen["jobs_run_on_start"] || !seen["group_policy"] {
		t.Fatal("metadata options missing keys")
	}
}

func TestConfigVisionDefaultAndBoolean(t *testing.T) {
	cfg := mustConfig(t, nil)
	if cfg.VisionEnabled {
		t.Fatal("vision must default false")
	}
	cfg = mustConfig(t, map[string]any{"vision_enabled": true, "memory_enabled": false})
	if !cfg.VisionEnabled || cfg.MemoryEnabled {
		t.Fatal("vision true / memory false not accepted")
	}
}

func TestConfigRejectsMalformedScalarsAndNormalizes(t *testing.T) {
	cases := []struct {
		name string
		raw  map[string]any
		want string
	}{
		{"bool", map[string]any{"vision_enabled": "maybe"}, "chat-agent: 配置错误 vision_enabled=maybe: 必须是布尔值"},
		{"duration", map[string]any{"batch_window": "abc"}, "chat-agent: 配置错误 batch_window=abc: 必须是时长"},
		{"int", map[string]any{"reply_max_chars": "abc"}, "chat-agent: 配置错误 reply_max_chars=abc: 必须是整数"},
		{"number", map[string]any{"llm_temperature": "NaN"}, "chat-agent: 配置错误 llm_temperature=NaN: 必须是数字"},
		{"string", map[string]any{"llm_model": 123}, "chat-agent: 配置错误 llm_model=123: 必须是字符串"},
		{"url host", map[string]any{"llm_base_url": "http://:8080"}, "chat-agent: 配置错误 llm_base_url=http://:8080: 必须是带 host 的 http(s) URL"},
		{"platform list", map[string]any{"group_list": []string{"telegram:"}}, "chat-agent: 配置错误 group_list=[telegram:]: 条目必须是 platform:id"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw := map[string]any{"llm_model": "m", "embedding_model": "e"}
			for k, v := range tc.raw {
				raw[k] = v
			}
			_, err := loadConfig(bot.NewConfig(raw))
			if err == nil || err.Error() != tc.want {
				t.Fatalf("err=%v, want %q", err, tc.want)
			}
		})
	}

	cfg := mustConfig(t, map[string]any{
		"llm_base_url": " https://api.example.com/v1 ",
		"llm_model":    " model ",
		"super_admins": []string{"telegram: 42"},
		"group_list":   []string{" telegram : -1 "},
		"self_ids":     []string{"telegram: bot "},
	})
	if cfg.LLMBaseURL != "https://api.example.com/v1" || cfg.LLMModel != "model" {
		t.Fatalf("string trim: base=%q model=%q", cfg.LLMBaseURL, cfg.LLMModel)
	}
	if got := strings.Join(cfg.SuperAdmins, ","); got != "telegram:42" {
		t.Fatalf("admins=%q", got)
	}
	if got := strings.Join(cfg.GroupList, ","); got != "telegram:-1" {
		t.Fatalf("group list=%q", got)
	}
	if got := strings.Join(cfg.SelfIDs, ","); got != "telegram:bot" {
		t.Fatalf("self ids=%q", got)
	}
}
