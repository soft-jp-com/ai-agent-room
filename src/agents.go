package main

// 参加エージェントの構成。既定の3つ（claude / codex / agy）に加えて、同じ種類のエージェントを
// 追加で登録できる（例: Codex の利用枠が尽きたときに2つ目の Claude Code を入れる）。
// 構成は logs/agents.json に保持し、起動時に読み込む。既定の3つは削除できない。

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
)

// agentType は CLI の種類ごとの既定値
type agentType struct {
	Name    string
	Color   string
	Aliases []string
	New     func() Adapter
}

var agentTypes = map[string]agentType{
	"claude": {"Claude Code", "#d97757", []string{"claude"}, func() Adapter { return newClaudeAdapter() }},
	"codex":  {"Codex CLI", "#10a37f", []string{"codex"}, func() Adapter { return newCodexAdapter() }},
	"agy":    {"Antigravity", "#4285f4", []string{"agy", "antigravity", "gemini"}, func() Adapter { return newAgyAdapter() }},
}

// agentTypeOrder は既定のエージェントの並び（ID は種類名と同じ）
var agentTypeOrder = []string{"claude", "codex", "agy"}

// maxAgentsPerType は同じ種類のエージェントの上限（既定の1つを含む）
const maxAgentsPerType = 5

var (
	ErrInvalidAgentType = errors.New("エージェントの種類が不正です")
	ErrAgentsBusy       = errors.New("会話の進行中はエージェントを追加・削除できません。停止してから操作してください")
	ErrAgentLimit       = errors.New("同じ種類のエージェントはこれ以上追加できません")
	ErrAgentDefault     = errors.New("既定のエージェントは削除できません")
)

// agentSpec は logs/agents.json に保存するエージェント1つ分
type agentSpec struct {
	ID   string `json:"id"`
	Type string `json:"type"`
	Name string `json:"name"`
}

func agentsPath(logDir string) string { return filepath.Join(logDir, "agents.json") }

// newAgent は構成からエージェントを作る。追加分は ID だけを別名にする（種類名で呼ぶと既定のエージェントに届く）
func newAgent(s agentSpec) *Agent {
	t := agentTypes[s.Type]
	aliases := t.Aliases
	if s.ID != s.Type {
		aliases = []string{s.ID}
	}
	return &Agent{ID: s.ID, Type: s.Type, Name: s.Name, Color: t.Color, Aliases: aliases, Adapter: t.New()}
}

// loadAgents は既定の3つと、logs/agents.json に保存された追加分のエージェントを作る
func loadAgents(logDir string) ([]*Agent, error) {
	var agents []*Agent
	for _, typ := range agentTypeOrder {
		agents = append(agents, newAgent(agentSpec{ID: typ, Type: typ, Name: agentTypes[typ].Name}))
	}
	b, err := os.ReadFile(agentsPath(logDir))
	if errors.Is(err, os.ErrNotExist) {
		return agents, nil
	}
	if err != nil {
		return agents, err
	}
	var specs []agentSpec
	if err := json.Unmarshal(b, &specs); err != nil {
		return agents, err
	}
	seen := map[string]bool{"claude": true, "codex": true, "agy": true}
	for _, s := range specs {
		if _, ok := agentTypes[s.Type]; !ok || seen[s.ID] || !extraIDRe(s.Type, s.ID) {
			continue // 既定のエージェントと不正な記録は読み飛ばす
		}
		seen[s.ID] = true
		agents = append(agents, newAgent(s))
	}
	return agents, nil
}

// extraIDRe は追加分の ID が「種類名+2以上の番号」か確かめる
func extraIDRe(typ, id string) bool {
	if len(id) <= len(typ) || id[:len(typ)] != typ {
		return false
	}
	n, err := strconv.Atoi(id[len(typ):])
	return err == nil && n >= 2 && n <= maxAgentsPerType && strconv.Itoa(n) == id[len(typ):]
}

func (r *Room) saveAgentsLocked() {
	var specs []agentSpec
	for _, a := range r.agents {
		if a.ID != a.Type {
			specs = append(specs, agentSpec{ID: a.ID, Type: a.Type, Name: a.Name})
		}
	}
	b, _ := json.MarshalIndent(specs, "", "  ")
	if err := writeStateFile(agentsPath(r.logDir), b); err != nil {
		r.log.Warn("agents.save", "error", err.Error())
	}
}

// agentsBusyLocked は、エージェントの構成を変えると進行中の処理と食い違う状態かを返す
func (r *Room) agentsBusyLocked() bool {
	if r.disc != nil || r.free != nil || r.summarizing || len(r.queue) > 0 || r.waiting != nil {
		return true
	}
	for _, a := range r.agents {
		if a.state == "thinking" {
			return true
		}
	}
	return false
}

// AddAgent は指定した種類のエージェントを1つ追加する。ID は「種類名+番号」（例: claude2）
func (r *Room) AddAgent(typ string) (*Agent, error) {
	t, ok := agentTypes[typ]
	if !ok {
		return nil, ErrInvalidAgentType
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.agentsBusyLocked() {
		return nil, ErrAgentsBusy
	}
	n := 2
	for ; n <= maxAgentsPerType && r.agent(typ+strconv.Itoa(n)) != nil; n++ {
	}
	if n > maxAgentsPerType {
		return nil, ErrAgentLimit
	}
	a := newAgent(agentSpec{ID: typ + strconv.Itoa(n), Type: typ, Name: fmt.Sprintf("%s %d", t.Name, n)})
	a.state = "idle"
	if !a.Adapter.Available() {
		a.state = "unavailable"
	}
	// 反復中の読み手と配列を共有しないよう、作り直して差し替える
	r.agents = append(append([]*Agent{}, r.agents...), a)
	r.saveAgentsLocked()
	r.saveSessionLocked()
	r.log.Info("agents.add", "agent", a.ID, "type", typ, "available", a.state != "unavailable")
	r.postLocked("system", fmt.Sprintf("%s（@%s）を追加しました。", a.Name, a.ID), "system")
	r.pushStatusLocked()
	go r.refreshQuota(a, true)
	return a, nil
}

// RemoveAgent は追加したエージェントを削除する。既定の3つは削除できない
func (r *Room) RemoveAgent(id string) error {
	if err := r.removeAgent(id); err != nil {
		return err
	}
	// 設定からも消す（保存したモデル・応答の上限、進行役の指定）。同じ ID で追加し直したときに前の設定が戻らないようにする
	r.SaveSettings()
	return nil
}

func (r *Room) removeAgent(id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	a := r.agent(id)
	if a == nil {
		return ErrAgentNotFound
	}
	if a.ID == a.Type {
		return ErrAgentDefault
	}
	if r.agentsBusyLocked() {
		return ErrAgentsBusy
	}
	agents := make([]*Agent, 0, len(r.agents)-1)
	for _, o := range r.agents {
		if o != a {
			agents = append(agents, o)
		}
	}
	r.agents = agents
	if r.leader == id {
		r.leader = ""
	}
	r.saveAgentsLocked()
	r.saveSessionLocked()
	r.saveUsageLocked()
	r.log.Info("agents.remove", "agent", id)
	r.postLocked("system", fmt.Sprintf("%s（@%s）を削除しました。", a.Name, a.ID), "system")
	r.pushStatusLocked()
	return nil
}

// agentsSnapshot はロックの外で全エージェントを回すための写しを返す
func (r *Room) agentsSnapshot() []*Agent {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]*Agent{}, r.agents...)
}
