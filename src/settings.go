package main

// 設定のサーバ側保存（案 7）。
// 間隔・上限・セッション切替・進行役などを保存し、起動時に読み込む。
// 保存先は設定フォルダ（エージェントが書き換えられない場所。protect.go）。設定フォルダが決まっていなければログディレクトリ。
// 以前のログディレクトリの settings.json は、読み込んだあと設定フォルダへ移す。
// ブラウザを変えても、再起動しても設定が保たれる。進め方・周回は画面だけの設定なので、ここには入れない。

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"time"
)

type roomSettings struct {
	MaxHops      *int    `json:"max_hops,omitempty"`
	DelaySec     *int    `json:"delay_sec,omitempty"`
	RotateTokens *int    `json:"rotate_tokens,omitempty"`
	Leader       *string `json:"leader,omitempty"`
	// CommandLeaderOnly は、進行役（と人間）の発言のコードブロックだけを実行できるようにする（修正案 7.1）
	CommandLeaderOnly *bool `json:"command_leader_only,omitempty"`
	// TurnTimeoutSec は1ターンの上限時間（秒）。AgentTimeoutSec はエージェントごとの上書き（案 8.3）
	TurnTimeoutSec  *int           `json:"turn_timeout_sec,omitempty"`
	AgentTimeoutSec map[string]int `json:"agent_timeout_sec,omitempty"`
	// AgentModel は人間が画面で選んだモデル（エージェント ID ごと。既定のモデルは入れない）（案2）
	AgentModel map[string]string `json:"agent_model,omitempty"`
	// AgentPermission は人間が選んだ権限の段階（エージェント ID ごと。既定は入れない）（案8）
	AgentPermission map[string]string `json:"agent_permission,omitempty"`
}

func (r *Room) settingsPath() string {
	if r.configDir == "" {
		return r.legacySettingsPath()
	}
	// 同じ PC で別のログディレクトリの AI Agent Room を動かしても混ざらないよう、ログディレクトリごとに分ける
	return filepath.Join(r.configDir, "settings-"+logDirKey(r.logDir)+".json")
}

// legacySettingsPath は以前の保存先（エージェントが書き換えられるログディレクトリ）
func (r *Room) legacySettingsPath() string { return filepath.Join(r.logDir, "settings.json") }

// SetConfigDir は設定フォルダを設定する（起動時、LoadSettings より前に呼ぶ）
func (r *Room) SetConfigDir(dir string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.configDir = dir
}

// SaveSettings は今の設定を保存する。
func (r *Room) SaveSettings() {
	r.mu.Lock()
	maxHops, delay, rotate, leader, leaderOnly := r.maxHops, int(r.delay/time.Second), r.rotateTokens, r.leader, r.commandLeaderOnly
	timeout := int(r.turnTimeout / time.Second)
	var agentTimeout map[string]int
	var agentModel map[string]string
	var agentPerm map[string]string
	for _, a := range r.agents {
		if a.timeoutSec > 0 {
			if agentTimeout == nil {
				agentTimeout = map[string]int{}
			}
			agentTimeout[a.ID] = a.timeoutSec
		}
		if a.humanModel != "" {
			if agentModel == nil {
				agentModel = map[string]string{}
			}
			agentModel[a.ID] = a.humanModel
		}
		if a.permission != permDefault {
			if agentPerm == nil {
				agentPerm = map[string]string{}
			}
			agentPerm[a.ID] = a.permission
		}
	}
	r.mu.Unlock()
	b, _ := json.MarshalIndent(roomSettings{MaxHops: &maxHops, DelaySec: &delay, RotateTokens: &rotate, Leader: &leader, CommandLeaderOnly: &leaderOnly,
		TurnTimeoutSec: &timeout, AgentTimeoutSec: agentTimeout, AgentModel: agentModel, AgentPermission: agentPerm}, "", "  ")
	path := r.settingsPath()
	os.MkdirAll(filepath.Dir(path), 0o700)
	if err := writeStateFile(path, b); err != nil {
		r.log.Warn("settings.save", "error", err.Error())
	}
}

// LoadSettings は保存した設定を読み込んで反映する。skip に含む項目（コマンドラインで指定したもの）は反映しない。
// 使えなくなった進行役は反映しない。
func (r *Room) LoadSettings(skip map[string]bool) {
	path := r.settingsPath()
	b, err := os.ReadFile(path)
	migrate := false
	if os.IsNotExist(err) && path != r.legacySettingsPath() {
		if b, err = os.ReadFile(r.legacySettingsPath()); err == nil {
			migrate = true
		}
	}
	if err != nil {
		if !os.IsNotExist(err) {
			r.log.Warn("settings.load", "error", err.Error())
		}
		return
	}
	var s roomSettings
	if err := json.Unmarshal(b, &s); err != nil {
		r.log.Warn("settings.load", "error", err.Error())
		return
	}
	if s.MaxHops != nil && !skip["max_hops"] {
		r.SetMaxHops(*s.MaxHops)
	}
	if s.DelaySec != nil && !skip["delay_sec"] {
		r.SetDelay(*s.DelaySec)
	}
	if s.RotateTokens != nil {
		r.SetRotateTokens(*s.RotateTokens)
	}
	if s.Leader != nil {
		if err := r.SetLeader(*s.Leader); err != nil {
			r.log.Warn("settings.load.leader", "leader", *s.Leader, "error", err.Error())
		}
	}
	if s.CommandLeaderOnly != nil {
		r.SetCommandLeaderOnly(*s.CommandLeaderOnly)
	}
	if s.TurnTimeoutSec != nil {
		r.SetTurnTimeout(*s.TurnTimeoutSec)
	}
	for id, sec := range s.AgentTimeoutSec { // 消えたエージェントの設定は読み飛ばす
		if err := r.SetAgentTimeout(id, sec); err != nil {
			r.log.Warn("settings.load.agent_timeout", "agent", id, "error", err.Error())
		}
	}
	for id, model := range s.AgentModel {
		r.restoreAgentModel(id, model)
	}
	for id, level := range s.AgentPermission {
		r.restoreAgentPermission(id, level)
	}
	if migrate {
		r.SaveSettings()
		if _, err := os.Stat(path); err == nil {
			os.Remove(r.legacySettingsPath())
			r.log.Info("settings.migrate", "to", path)
		}
	}
	r.log.Info("settings.load")
}

// SetCommandLeaderOnly は、コードブロックを実行できる発言を進行役（と人間）のものに限るかを設定する
func (r *Room) SetCommandLeaderOnly(on bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.commandLeaderOnly = on
	r.pushStatusLocked()
}

// restoreAgentModel は保存した人間のモデル選択を戻す（案2）。消えたエージェントや、候補にないモデルは戻さずにログを出す。
// 起動時の復元なので、チャットにはシステムメッセージを出さない
func (r *Room) restoreAgentModel(id, model string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	a := r.agent(id)
	if a == nil {
		r.log.Warn("settings.load.agent_model", "agent", id, "model", model, "error", ErrAgentNotFound.Error())
		return
	}
	if !slices.ContainsFunc(a.Adapter.Models(), func(o ModelOption) bool { return o.ID == model }) {
		r.log.Warn("settings.load.agent_model", "agent", id, "model", model, "error", "候補にないモデルです")
		return
	}
	a.modelSel, a.humanModel = model, model
	a.modelVer++
	r.log.Info("agent.model.set", "agent", a.ID, "model", model, "by", "settings", "model_ver", a.modelVer)
	r.pushStatusLocked()
}
