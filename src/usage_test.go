package main

import "testing"

// 案15: status の各エージェントに、トークン使用量の累計と直近1回の入力が載る。実行の記録がなければ載せない
func TestAgentStatusUsage(t *testing.T) {
	r, a := newTestRoom(t)
	r.mu.Lock()
	defer r.mu.Unlock()

	if s := r.statusLocked().Agents[0]; s.Usage != nil || s.LastInput != 0 {
		t.Fatalf("記録がないのに使用量が載った: %#v", s)
	}
	a.usage.add(Usage{Known: true, Input: 1000, CachedInput: 800, Output: 50})
	a.usage.add(Usage{}) // 取得できなかった実行
	a.lastInput = 1000
	s := r.statusLocked().Agents[0]
	if s.Usage == nil || s.Usage.Jobs != 2 || s.Usage.UnknownJobs != 1 || s.Usage.Input != 1000 || s.Usage.CachedInput != 800 || s.Usage.Output != 50 || s.LastInput != 1000 {
		t.Fatalf("使用量 %#v / 直近 %d", s.Usage, s.LastInput)
	}
	// status の値は写し。あとで累計が増えても、配信済みの値は変わらない
	a.usage.add(Usage{Known: true, Input: 1})
	if s.Usage.Input != 1000 {
		t.Fatalf("配信済みの使用量が変わった: %d", s.Usage.Input)
	}
}
