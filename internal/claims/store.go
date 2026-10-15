// Package claims 持久化每日签到结果（data/claims.json），
// 供调度器写入、/claims 端点读取。线程安全，原子落盘。
package claims

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// Record 单个账号一天的签到结果。
type Record struct {
	OK  bool   `json:"ok"`
	At  string `json:"at"`            // 执行时间 RFC3339
	Msg string `json:"msg,omitempty"` // 上游返回摘要或错误（已截断）
}

// Store 签到记录存储。Days[日期][uid] = Record。
type Store struct {
	mu   sync.Mutex
	fp   string
	Days map[string]map[string]Record `json:"days"`
}

const keepDays = 30

// nowDay 当前日期 key（"2006-01-02"），本地时区。
func nowDay() string { return time.Now().Format("2006-01-02") }

// Load 读取签到记录；文件不存在返回空 Store。
func Load(fp string) *Store {
	s := &Store{fp: fp, Days: map[string]map[string]Record{}}
	raw, err := os.ReadFile(fp)
	if err != nil {
		return s
	}
	var out Store
	if err := json.Unmarshal(raw, &out); err != nil {
		return s // 损坏则重置，不阻塞服务
	}
	if out.Days == nil {
		out.Days = map[string]map[string]Record{}
	}
	out.fp = fp
	return &out
}

// Record 写入某账号当天的签到结果并落盘。
func (s *Store) Record(uid string, ok bool, msg string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	day := nowDay()
	if s.Days[day] == nil {
		s.Days[day] = map[string]Record{}
	}
	if len(msg) > 300 {
		msg = msg[:300]
	}
	s.Days[day][uid] = Record{OK: ok, At: time.Now().Format(time.RFC3339), Msg: msg}
	s.pruneLocked()
	s.saveLocked()
}

// Snapshot 返回最近 days 天的记录副本（降序日期），供 API 展示。
func (s *Store) Snapshot(days int) map[string]map[string]Record {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]map[string]Record{}
	dates := make([]string, 0, len(s.Days))
	for d := range s.Days {
		dates = append(dates, d)
	}
	sort.Sort(sort.Reverse(sort.StringSlice(dates)))
	if days > 0 && len(dates) > days {
		dates = dates[:days]
	}
	for _, d := range dates {
		m := map[string]Record{}
		for uid, r := range s.Days[d] {
			m[uid] = r
		}
		out[d] = m
	}
	return out
}

// ClaimedToday 该账号今天是否已成功签到（启动补签去重）。
func (s *Store) ClaimedToday(uid string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	day := nowDay()
	if m := s.Days[day]; m != nil {
		r, ok := m[uid]
		return ok && r.OK
	}
	return false
}

// pruneLocked 清理超过 keepDays 的记录。调用方需持锁。
func (s *Store) pruneLocked() {
	cutoff := time.Now().AddDate(0, 0, -keepDays).Format("2006-01-02")
	for d := range s.Days {
		if d < cutoff {
			delete(s.Days, d)
		}
	}
}

// saveLocked 原子写盘。调用方需持锁。
func (s *Store) saveLocked() {
	if s.fp == "" {
		return
	}
	raw, err := json.MarshalIndent(struct {
		Days map[string]map[string]Record `json:"days"`
	}{Days: s.Days}, "", "  ")
	if err != nil {
		return
	}
	if dir := filepath.Dir(s.fp); dir != "" {
		_ = os.MkdirAll(dir, 0o755)
	}
	tmp := s.fp + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return
	}
	_ = os.Rename(tmp, s.fp)
}
