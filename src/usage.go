package main

// エージェントごとのトークン使用量の累計。ジョブ（CLI の1回の起動）単位で集計し、
// logs/usage.json に保持する。[pass]・破棄・キャンセルされたジョブも消費に含める。
// 使用量を取得できなかったジョブはゼロ扱いせず、件数（UnknownJobs）として別に数える。

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

type UsageTotal struct {
	Jobs        int    `json:"jobs"`         // 集計したジョブ数（取得できなかったものを含む）
	UnknownJobs int    `json:"unknown_jobs"` // 使用量を取得できなかったジョブ数
	Input       int    `json:"input"`        // 取得済み分の入力トークン合計（キャッシュ分を含む）
	CachedInput int    `json:"cached_input"` // うちキャッシュから読んだ分
	Output      int    `json:"output"`       // 取得済み分の出力トークン合計
	Updated     string `json:"updated"`      // 最終更新時刻（ローカル時刻）
}

func (t *UsageTotal) add(u Usage) {
	t.Jobs++
	if u.Known {
		t.Input += u.Input
		t.CachedInput += u.CachedInput
		t.Output += u.Output
	} else {
		t.UnknownJobs++
	}
	t.Updated = time.Now().Format("2006-01-02 15:04:05")
}

// summary はプロンプトに添える利用状況の文面
func (t UsageTotal) summary() string {
	if t.Jobs == 0 {
		return "記録なし"
	}
	if t.Jobs == t.UnknownJobs {
		return fmt.Sprintf("不明（%d回の実行すべてで取得できず）", t.Jobs)
	}
	s := fmt.Sprintf("%d回の実行で 入力 %d（うちキャッシュ %d）/ 出力 %d トークン", t.Jobs, t.Input, t.CachedInput, t.Output)
	if t.UnknownJobs > 0 {
		s += fmt.Sprintf("、ほかに取得できなかった実行 %d回", t.UnknownJobs)
	}
	return s
}

// usageFile は使用量を保持する専用ファイルの内容
type usageFile struct {
	Agents map[string]UsageTotal `json:"agents"`
	Quotas map[string]Quota      `json:"quotas,omitempty"` // プランの残り利用枠（取得できた CLI のみ）
}

func (r *Room) usagePath() string { return filepath.Join(r.logDir, "usage.json") }

// loadUsageLocked は前回までの累計を読み込む。ファイルがなければ0から始める
func (r *Room) loadUsageLocked() {
	b, err := os.ReadFile(r.usagePath())
	if err != nil {
		return
	}
	var f usageFile
	if err := json.Unmarshal(b, &f); err != nil {
		r.log.Warn("usage.load", "error", err.Error())
		return
	}
	for _, a := range r.agents {
		a.usage = f.Agents[a.ID]
		a.quota = f.Quotas[a.ID]
	}
}

func (r *Room) saveUsageLocked() {
	f := usageFile{Agents: map[string]UsageTotal{}, Quotas: map[string]Quota{}}
	for _, a := range r.agents {
		f.Agents[a.ID] = a.usage
		if a.quota.has() || a.quota.Error != "" {
			f.Quotas[a.ID] = a.quota
		}
	}
	b, _ := json.MarshalIndent(f, "", "  ")
	if err := writeStateFile(r.usagePath(), b); err != nil {
		r.log.Warn("usage.save", "error", err.Error())
	}
}
