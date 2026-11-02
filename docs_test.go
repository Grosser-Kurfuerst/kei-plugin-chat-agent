package chatagent

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"unicode"
)

func readDoc(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("docs", name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(data)
}

func TestDocsConfigKeySets(t *testing.T) {
	cfgDoc := readDoc(t, "configuration.md")
	keyRE := regexp.MustCompile("(?m)^\\| `([a-z0-9_]+)` \\|")
	var docKeys []string
	for _, m := range keyRE.FindAllStringSubmatch(cfgDoc, -1) {
		docKeys = append(docKeys, m[1])
	}
	sort.Strings(docKeys)
	got := append([]string(nil), metadataOptions...)
	sort.Strings(got)
	if strings.Join(docKeys, "\n") != strings.Join(got, "\n") {
		t.Fatalf("configuration keys (%d) != Metadata.Options (%d)\ndoc=%v\nmeta=%v",
			len(docKeys), len(got), docKeys, got)
	}
	if len(docKeys) != 82 {
		t.Fatalf("configuration keys = %d, want 82", len(docKeys))
	}

	arch := readDoc(t, "architecture.md")
	start := strings.Index(arch, "Options: []string{")
	if start < 0 {
		t.Fatal("architecture Options block missing")
	}
	end := strings.Index(arch[start:], "\n}")
	if end < 0 {
		t.Fatal("architecture Options block end missing")
	}
	block := arch[start : start+end]
	optRE := regexp.MustCompile(`"([a-z0-9_]+)"`)
	var archKeys []string
	for _, m := range optRE.FindAllStringSubmatch(block, -1) {
		archKeys = append(archKeys, m[1])
	}
	sort.Strings(archKeys)
	if strings.Join(archKeys, "\n") != strings.Join(got, "\n") {
		t.Fatalf("architecture Options (%d) != Metadata.Options (%d)", len(archKeys), len(got))
	}

	start = strings.Index(cfgDoc, "以下清单是 `Metadata.Options` 的镜像")
	if start < 0 {
		t.Fatal("configuration mirror list missing")
	}
	block = cfgDoc[start:]
	open := strings.Index(block, "```text\n")
	if open < 0 {
		t.Fatal("configuration mirror block missing")
	}
	block = block[open+len("```text\n"):]
	closeAt := strings.Index(block, "\n```")
	if closeAt < 0 {
		t.Fatal("configuration mirror block unterminated")
	}
	var mirrorKeys []string
	for _, line := range strings.Split(block[:closeAt], "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			mirrorKeys = append(mirrorKeys, line)
		}
	}
	sort.Strings(mirrorKeys)
	if strings.Join(mirrorKeys, "\n") != strings.Join(got, "\n") {
		t.Fatalf("configuration mirror (%d) != Metadata.Options (%d)", len(mirrorKeys), len(got))
	}
}

func TestDocsIndexAndChapters(t *testing.T) {
	files, err := filepath.Glob("docs/*.md")
	if err != nil {
		t.Fatal(err)
	}
	anchors := map[string]map[string]bool{}
	headingRE := regexp.MustCompile(`(?m)^#{1,6} (.+)$`)
	for _, file := range files {
		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		seen := map[string]int{}
		set := map[string]bool{}
		for _, m := range headingRE.FindAllStringSubmatch(string(data), -1) {
			base := githubAnchor(m[1])
			slug := base
			if n := seen[base]; n > 0 {
				slug = base + "-" + strconv.Itoa(n)
			}
			seen[base]++
			set[slug] = true
		}
		anchors[filepath.Base(file)] = set
	}
	readme := readDoc(t, "README.md")
	linkRE := regexp.MustCompile(`\]\(([a-zA-Z0-9_.-]+\.md)(?:#([^)]+))?\)`)
	for _, file := range files {
		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range linkRE.FindAllStringSubmatch(string(data), -1) {
			if _, err := os.Stat(filepath.Join("docs", m[1])); err != nil {
				t.Fatalf("%s links missing doc %s", file, m[1])
			}
			if m[2] != "" && !anchors[m[1]][m[2]] {
				t.Fatalf("%s links missing anchor %s#%s", file, m[1], m[2])
			}
		}
		if filepath.Base(file) == "README.md" {
			continue
		}
		if !strings.Contains(string(data), "覆盖：") || !strings.Contains(string(data), "相关文档") {
			t.Fatalf("%s missing coverage/relevant sections", file)
		}
		if !strings.Contains(readme, "("+filepath.Base(file)+")") {
			t.Fatalf("%s missing from README index", filepath.Base(file))
		}
	}
	chapterRE := regexp.MustCompile(`(?m)^## (\d+)\.`)
	seen := map[int]string{}
	for _, file := range files {
		data, _ := os.ReadFile(file)
		for _, m := range chapterRE.FindAllStringSubmatch(string(data), -1) {
			n, _ := strconv.Atoi(m[1])
			if prev, ok := seen[n]; ok {
				t.Fatalf("chapter %d duplicated in %s and %s", n, prev, file)
			}
			seen[n] = file
		}
	}
	for i := 1; i <= 16; i++ {
		if seen[i] == "" {
			t.Fatalf("chapter %d missing", i)
		}
	}
	if len(seen) != 16 {
		t.Fatalf("chapters = %d, want 16", len(seen))
	}
}

func githubAnchor(heading string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(heading) {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r) || unicode.IsMark(r):
			b.WriteRune(r)
		case r == '-' || r == '_':
			b.WriteRune(r)
		case r == ' ':
			b.WriteByte('-')
		}
	}
	return b.String()
}

func TestDocsFixedLiterals(t *testing.T) {
	conv := readDoc(t, "conversation.md")
	if !strings.Contains(conv, "你是 {{bot_name}}，一个聊天助手。当前会话是{{chat_kind}}") {
		t.Fatal("default system prompt drifted in docs")
	}
	mem := readDoc(t, "memory.md")
	if !strings.Contains(mem, "你是记忆整理器。") {
		t.Fatal("memory extraction prompt drifted in docs")
	}
	part := readDoc(t, "participation.md")
	if !strings.Contains(part, "empty_text, no_sender") {
		t.Fatal("reason vocabulary drifted in docs")
	}
	admin := readDoc(t, "admin.md")
	if !strings.Contains(admin, "chat-agent: 无权执行该命令") {
		t.Fatal("admin denial line drifted in docs")
	}
	arch := readDoc(t, "architecture.md")
	for _, lit := range []string{"chat-agent: 需要 TableStorage 支持", "chat-agent: 存储表初始化失败", "chat-agent: 存储表加载失败", `"importance"`} {
		if !strings.Contains(arch, lit) {
			t.Fatalf("architecture missing %q", lit)
		}
	}
}

func TestDocsPromptLiteralsExact(t *testing.T) {
	extract := func(doc, heading string) string {
		t.Helper()
		idx := strings.Index(doc, heading)
		if idx < 0 {
			t.Fatalf("heading %q missing", heading)
		}
		rest := doc[idx:]
		start := strings.Index(rest, "```text\n")
		if start < 0 {
			t.Fatalf("text block after %q missing", heading)
		}
		rest = rest[start+len("```text\n"):]
		end := strings.Index(rest, "\n```")
		if end < 0 {
			t.Fatalf("text block after %q unterminated", heading)
		}
		return rest[:end]
	}
	conv := readDoc(t, "conversation.md")
	if got := extract(conv, "#### 默认系统提示词"); got != defaultSystemPrompt {
		t.Fatalf("default system prompt mismatch:\n--- doc\n%s\n--- code\n%s", got, defaultSystemPrompt)
	}
	mem := readDoc(t, "memory.md")
	if got := extract(mem, "### 9.2 抽取"); got != memoryExtractPrompt {
		t.Fatalf("extraction prompt mismatch:\n--- doc\n%s\n--- code\n%s", got, memoryExtractPrompt)
	}
}

func TestMetadataOptionsUniqueAndCount(t *testing.T) {
	if len(metadataOptions) != 82 {
		t.Fatalf("options=%d", len(metadataOptions))
	}
	seen := map[string]bool{}
	for _, k := range metadataOptions {
		if seen[k] {
			t.Fatalf("duplicate %s", k)
		}
		seen[k] = true
	}
}
