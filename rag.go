package chatagent

import (
	"context"
	"fmt"
	"strings"
)

// ragResult 是一次 RAG 检索的注入结果。
type ragResult struct {
	MemoryBlock    string
	KnowledgeBlock string
	Status         string
}

// ragLine 是注入块中的一行及其分数。
type ragLine struct {
	Score float64
	Text  string
}

// retrieveRAG 执行记忆与知识库双库检索，并按预算渲染注入块。
func (p *Plugin) retrieveRAG(ctx context.Context, s *sessionState, batch []Turn, query string) ragResult {
	if !p.cfg.MemoryEnabled && !p.cfg.RAGEnabled {
		return ragResult{Status: "disabled"}
	}
	if p.cfg.RAGContextMaxChars == 0 {
		return ragResult{Status: "disabled"}
	}
	memoryActive := p.cfg.MemoryEnabled && p.cfg.MemoryTopK > 0 && p.cfg.MemoryContextMaxChars > 0
	knowledgeActive := p.cfg.RAGEnabled && p.cfg.KnowledgeTopK > 0 && p.cfg.KnowledgeContextMaxChars > 0
	if !memoryActive && !knowledgeActive {
		return ragResult{Status: "disabled"}
	}
	state := p.idx.waitReady(ctx, vectorReadyTimeout)
	if state == vectorStateDisabled {
		return ragResult{Status: "disabled"}
	}
	if state == vectorStateLoading {
		return ragResult{Status: "loading"}
	}
	query = takePrefix(strings.TrimSpace(query), contextMessageMaxChars)
	if query == "" {
		return ragResult{Status: "empty"}
	}
	vecs, err := p.embedder.Embed(ctx, []string{query})
	if err != nil {
		p.log.Warn("rag embedding failed", "operation", "generate", "reason", "rag_status", "error", err.Error())
		return ragResult{Status: "error"}
	}
	if len(vecs) != 1 {
		return ragResult{Status: "error"}
	}
	queryVec, err := normalizeVector(vecs[0])
	if err != nil {
		return ragResult{Status: "error"}
	}

	userIDs := make(map[string]struct{})
	for _, t := range batch {
		if t.UserID != "" {
			userIDs[t.UserID] = struct{}{}
		}
	}
	channelID := ""
	if s != nil && s.kind != "private" {
		channelID = s.channelID
	}

	var memoryLines []ragLine
	if memoryActive {
		hits, mismatch := p.idx.searchMemories(queryVec, channelID, userIDs, p.cfg.MemoryTopK)
		if mismatch > 0 {
			p.log.Warn("memory search dimension mismatch", "operation", "vector", "skipped", mismatch)
		}
		ids := make([]string, 0, len(hits))
		now := p.now().UTC()
		for _, h := range hits {
			if h.Score < p.cfg.MemoryMinScore {
				continue
			}
			entry, ok := p.idx.memoryByID(h.ID)
			if !ok {
				continue
			}
			memoryLines = append(memoryLines, ragLine{
				Score: h.Score,
				Text:  fmt.Sprintf("- (%s, %.2f) %s", entry.Kind, entry.Importance, safeText(entry.Text)),
			})
			ids = append(ids, h.ID)
		}
		if len(ids) > 0 {
			p.idx.reinforce(ids, now)
		}
	}

	var knowledgeLines []ragLine
	if knowledgeActive {
		hits, mismatch := p.idx.searchChunks(queryVec, p.cfg.KnowledgeTopK)
		if mismatch > 0 {
			p.log.Warn("knowledge search dimension mismatch", "operation", "vector", "skipped", mismatch)
		}
		titles := make(map[string]string)
		for _, d := range p.idx.docSnapshot() {
			titles[d.ID] = d.Title
		}
		for _, h := range hits {
			if h.Score < p.cfg.KnowledgeMinScore {
				continue
			}
			chunk, ok := p.idx.chunkByID(h.ID)
			if !ok {
				continue
			}
			title := titles[chunk.DocID]
			if title == "" {
				title = chunk.DocID
			}
			knowledgeLines = append(knowledgeLines, ragLine{
				Score: h.Score,
				Text:  fmt.Sprintf("- [%s#%d] %s", safeText(title), chunk.Index, safeText(chunk.Text)),
			})
		}
	}

	memoryLines = fitBudget(memoryLines, p.cfg.MemoryContextMaxChars)
	knowledgeLines = fitBudget(knowledgeLines, p.cfg.KnowledgeContextMaxChars)
	total := sumLines(memoryLines) + sumLines(knowledgeLines)
	for total > p.cfg.RAGContextMaxChars {
		if len(knowledgeLines) > 0 {
			knowledgeLines = dropLowest(knowledgeLines)
		} else if len(memoryLines) > 0 {
			memoryLines = dropLowest(memoryLines)
		} else {
			break
		}
		total = sumLines(memoryLines) + sumLines(knowledgeLines)
	}

	res := ragResult{}
	if len(memoryLines) > 0 {
		res.MemoryBlock = renderBlock("<long_term_memory>", memoryLines, "</long_term_memory>")
	}
	if len(knowledgeLines) > 0 {
		res.KnowledgeBlock = renderBlock("<knowledge_base>", knowledgeLines, "</knowledge_base>")
	}
	if res.MemoryBlock == "" && res.KnowledgeBlock == "" {
		res.Status = "empty"
	} else {
		res.Status = "ok"
	}
	return res
}

// fitBudget 按分数顺序填充预算；剩余预算小于 40 rune 的条目丢弃。
func fitBudget(lines []ragLine, budget int) []ragLine {
	if budget <= 0 || len(lines) == 0 {
		return nil
	}
	out := make([]ragLine, 0, len(lines))
	used := 0
	for _, line := range lines {
		remaining := budget - used
		if remaining < 40 {
			break
		}
		text := line.Text
		if runeLen(text) > remaining {
			text = truncateEllipsis(text, remaining)
		}
		out = append(out, ragLine{Score: line.Score, Text: text})
		used += runeLen(text)
	}
	return out
}

func sumLines(lines []ragLine) int {
	total := 0
	for _, line := range lines {
		total += runeLen(line.Text)
	}
	return total
}

func dropLowest(lines []ragLine) []ragLine {
	if len(lines) == 0 {
		return lines
	}
	lowest := 0
	for i := 1; i < len(lines); i++ {
		if lines[i].Score < lines[lowest].Score {
			lowest = i
		}
	}
	return append(lines[:lowest], lines[lowest+1:]...)
}

func renderBlock(open string, lines []ragLine, close string) string {
	var b strings.Builder
	b.WriteString(open)
	for _, line := range lines {
		b.WriteString("\n")
		b.WriteString(line.Text)
	}
	b.WriteString("\n")
	b.WriteString(close)
	return b.String()
}

// ragQueryFromBatch 拼接本批用户文本并截断到 1000 rune。
func ragQueryFromBatch(batch []Turn) string {
	var b strings.Builder
	for i, t := range batch {
		if i > 0 {
			b.WriteString("\n")
		}
		b.WriteString(t.Text)
	}
	return takePrefix(b.String(), contextMessageMaxChars)
}
