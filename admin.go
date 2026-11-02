package chatagent

import (
	"context"
	"encoding/json"
	"sync"

	"github.com/RandomLemon/kei/pkg/bot"
)

// adminsJSON 是 `admins` KV 键的 JSON 形状。
type adminsJSON struct {
	SuperAdmins []string `json:"super_admins"`
}

// adminStore 保存插件超级管理员集合。
type adminStore struct {
	mu sync.RWMutex

	seeds   []string
	ordered []string
	set     map[string]struct{}
}

func newAdminStore(cfg *config) *adminStore {
	s := &adminStore{
		seeds: append([]string(nil), cfg.SuperAdmins...),
		set:   make(map[string]struct{}),
	}
	s.ordered = append(s.ordered, s.seeds...)
	for _, id := range s.ordered {
		s.set[id] = struct{}{}
	}
	return s
}

// load 读取 `admins` 覆盖；损坏时回落配置并 warn。
func (s *adminStore) load(ctx context.Context, p *Plugin) {
	raw, err := p.readKey(ctx, keyAdmins)
	if err != nil {
		p.log.Warn("admins load failed", "operation", "storage", "error", err.Error())
		return
	}
	if len(raw) == 0 {
		return
	}
	var v adminsJSON
	if err := json.Unmarshal(raw, &v); err != nil {
		p.log.Warn("admins override corrupt", "operation", "storage", "error", err.Error())
		return
	}
	for _, id := range v.SuperAdmins {
		if !validPlatformUser(id) {
			p.log.Warn("admins override invalid", "operation", "storage")
			return
		}
	}
	for i := range v.SuperAdmins {
		v.SuperAdmins[i] = normalizePlatformEntry(v.SuperAdmins[i])
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ordered = dedupeStrings(v.SuperAdmins)
	s.set = make(map[string]struct{}, len(s.ordered))
	for _, id := range s.ordered {
		s.set[id] = struct{}{}
	}
	if len(s.ordered) == 0 {
		p.log.Warn("no super admin configured; use /chat-admin to recover")
	}
}

// isSuper 判断事件发送者是否为插件超级管理员。
func (s *adminStore) isSuper(ev *bot.Event) bool {
	if ev == nil || ev.Sender == nil {
		return false
	}
	id := ev.Platform + ":" + ev.Sender.ID
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, ok := s.set[id]
	return ok
}

// list 返回管理员快照。
func (s *adminStore) list() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]string(nil), s.ordered...)
}

// add 添加管理员；重复添加视为成功。
func (s *adminStore) add(id string) bool {
	id = normalizePlatformEntry(id)
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.set[id]; ok {
		return false
	}
	s.set[id] = struct{}{}
	s.ordered = append(s.ordered, id)
	return true
}

// del 删除管理员；删除唯一管理员返回 false。
func (s *adminStore) del(id string) (removed bool, last bool) {
	id = normalizePlatformEntry(id)
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.set[id]; !ok {
		return false, false
	}
	if len(s.ordered) <= 1 {
		return false, true
	}
	delete(s.set, id)
	out := s.ordered[:0]
	for _, x := range s.ordered {
		if x != id {
			out = append(out, x)
		}
	}
	s.ordered = out
	return true, false
}

// reset 恢复配置种子值。
func (s *adminStore) reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ordered = append([]string(nil), s.seeds...)
	s.set = make(map[string]struct{}, len(s.ordered))
	for _, id := range s.ordered {
		s.set[id] = struct{}{}
	}
}

// persist 把当前管理员集合写入 KV（异步）。
func (p *Plugin) persistAdmins() {
	list := p.admins.list()
	if list == nil {
		list = []string{}
	}
	p.kvSet(keyAdmins, marshalJSON(adminsJSON{SuperAdmins: list}))
}
