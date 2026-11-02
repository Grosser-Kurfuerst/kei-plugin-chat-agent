package chatagent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
)

// policyJSON 是 `policy` KV 键的 JSON 形状。
type policyJSON struct {
	GroupMode   string   `json:"group_mode"`
	GroupList   []string `json:"group_list"`
	PrivateMode string   `json:"private_mode"`
	PrivateList []string `json:"private_list"`
}

// policyStore 保存名单模式与列表；注册表值来自配置，可被 Storage 覆盖。
type policyStore struct {
	mu sync.RWMutex

	seedGroupMode   string
	seedGroupList   []string
	seedPrivateMode string
	seedPrivateList []string

	groupMode   string
	groupList   []string
	privateMode string
	privateList []string
}

func newPolicyStore(cfg *config) *policyStore {
	return &policyStore{
		seedGroupMode:   cfg.GroupPolicy,
		seedGroupList:   append([]string(nil), cfg.GroupList...),
		seedPrivateMode: cfg.PrivatePolicy,
		seedPrivateList: append([]string(nil), cfg.PrivateList...),
		groupMode:       cfg.GroupPolicy,
		groupList:       append([]string(nil), cfg.GroupList...),
		privateMode:     cfg.PrivatePolicy,
		privateList:     append([]string(nil), cfg.PrivateList...),
	}
}

// load 读取 `policy` 覆盖；损坏或字段非法时逐级回落配置并 warn。
func (s *policyStore) load(ctx context.Context, p *Plugin) {
	raw, err := p.readKey(ctx, keyPolicy)
	if err != nil {
		p.log.Warn("policy load failed", "operation", "storage", "error", err.Error())
		return
	}
	if len(raw) == 0 {
		return
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		p.log.Warn("policy override corrupt", "operation", "storage", "error", err.Error())
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if rv, ok := fields["group_mode"]; ok {
		var mode string
		if err := json.Unmarshal(rv, &mode); err != nil || !validMode(mode) {
			p.log.Warn("policy override invalid", "operation", "storage", "field", "group_mode")
		} else {
			s.groupMode = mode
		}
	}
	if rv, ok := fields["private_mode"]; ok {
		var mode string
		if err := json.Unmarshal(rv, &mode); err != nil || !validMode(mode) {
			p.log.Warn("policy override invalid", "operation", "storage", "field", "private_mode")
		} else {
			s.privateMode = mode
		}
	}
	if rv, ok := fields["group_list"]; ok {
		if list, err := decodeStringList(rv); err != nil {
			p.log.Warn("policy override invalid", "operation", "storage", "field", "group_list")
		} else {
			s.groupList = list
		}
	}
	if rv, ok := fields["private_list"]; ok {
		if list, err := decodeStringList(rv); err != nil {
			p.log.Warn("policy override invalid", "operation", "storage", "field", "private_list")
		} else {
			s.privateList = list
		}
	}
}

func decodeStringList(raw json.RawMessage) ([]string, error) {
	var list []string
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil, err
	}
	if list == nil {
		list = []string{}
	}
	trimmed := make([]string, 0, len(list))
	for _, s := range list {
		s = normalizePlatformEntry(s)
		if s == "" {
			return nil, errors.New("empty list entry")
		}
		if platform, id, ok := strings.Cut(s, ":"); ok && (platform == "" || id == "") {
			return nil, errors.New("invalid platform entry")
		}
		trimmed = append(trimmed, s)
	}
	return dedupeStrings(trimmed), nil
}

func validMode(mode string) bool {
	switch mode {
	case "off", "open", "whitelist", "blacklist":
		return true
	default:
		return false
	}
}

// allows 判断平台与 ID 是否被当前名单允许。
func (s *policyStore) allows(kind, platform, id string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var mode string
	var list []string
	if kind == "group" {
		mode, list = s.groupMode, s.groupList
	} else {
		mode, list = s.privateMode, s.privateList
	}
	switch mode {
	case "off":
		return false
	case "open":
		return true
	case "whitelist":
		return listMatches(list, platform, id)
	case "blacklist":
		return !listMatches(list, platform, id)
	default:
		return false
	}
}

func listMatches(list []string, platform, id string) bool {
	for _, entry := range list {
		if p, uid, ok := strings.Cut(entry, ":"); ok {
			if p == platform && uid == id {
				return true
			}
			continue
		}
		if entry == id {
			return true
		}
	}
	return false
}

// modeFor 返回指定会话类型的有效模式。
func (s *policyStore) modeFor(kind string) string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if kind == "group" {
		return s.groupMode
	}
	return s.privateMode
}

// snapshot 返回名单的深拷贝快照。
func (s *policyStore) snapshot() policyJSON {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return policyJSON{
		GroupMode:   s.groupMode,
		GroupList:   append([]string{}, s.groupList...),
		PrivateMode: s.privateMode,
		PrivateList: append([]string{}, s.privateList...),
	}
}

// setMode 修改指定类型模式；调用方负责持久化。
func (s *policyStore) setMode(kind, mode string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if kind == "group" {
		s.groupMode = mode
	} else {
		s.privateMode = mode
	}
}

// addEntry 追加名单条目并返回是否实际新增。
func (s *policyStore) addEntry(kind, id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if kind == "group" {
		for _, e := range s.groupList {
			if e == id {
				return false
			}
		}
		s.groupList = append(s.groupList, id)
		return true
	}
	for _, e := range s.privateList {
		if e == id {
			return false
		}
	}
	s.privateList = append(s.privateList, id)
	return true
}

// delEntry 删除名单条目并返回是否存在。
func (s *policyStore) delEntry(kind, id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if kind == "group" {
		for i, e := range s.groupList {
			if e == id {
				s.groupList = append(s.groupList[:i], s.groupList[i+1:]...)
				return true
			}
		}
		return false
	}
	for i, e := range s.privateList {
		if e == id {
			s.privateList = append(s.privateList[:i], s.privateList[i+1:]...)
			return true
		}
	}
	return false
}

// reset 恢复配置种子值。
func (s *policyStore) reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.groupMode = s.seedGroupMode
	s.groupList = append([]string(nil), s.seedGroupList...)
	s.privateMode = s.seedPrivateMode
	s.privateList = append([]string(nil), s.seedPrivateList...)
}

// persist 把当前名单写入 KV（异步，不等待持久化）。
func (p *Plugin) persistPolicy() {
	snap := p.policy.snapshot()
	p.kvSet(keyPolicy, marshalJSON(snap))
}
