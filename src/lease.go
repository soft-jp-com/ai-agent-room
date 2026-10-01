package main

// 共有しているもの（ヘッドレス Chrome の CDP ポートなど）の貸し出し（修正案 7.3）。
// エージェントは MCP ツール lease_resource / release_resource で借りて返す。
// 使用中の一覧は status イベントの leases と、各エージェントへの依頼文に載せる。
// 返し忘れに備えて期限を付ける（既定30分）。期限を過ぎた貸し出しは、ほかのエージェントが借りられる。

import (
	"fmt"
	"slices"
	"strings"
	"time"
)

const (
	leaseToolName       = "lease_resource"
	releaseToolName     = "release_resource"
	leaseDefaultMinutes = 30
	leaseMaxMinutes     = 240
)

// selfToolNames は AI Agent Room の MCP ツールの一覧（CLI に承認不要として渡す）
var selfToolNames = []string{selfToolName, leaseToolName, releaseToolName}

// Lease は貸し出し1件
type Lease struct {
	Name   string `json:"name"`   // 共有しているものの名前（例: cdp-9333）
	Holder string `json:"holder"` // 借りているエージェントのID
	Until  int64  `json:"until"`  // 期限（UnixMilli）
}

// normalizeLeaseName は名前の前後の空白を除き、小文字にする（「CDP-9333」と「cdp-9333」を同じものとして扱う）
func normalizeLeaseName(name string) string { return strings.ToLower(strings.TrimSpace(name)) }

// expireLeasesLocked は期限を過ぎた貸し出しを外す
func (r *Room) expireLeasesLocked() {
	now := time.Now().UnixMilli()
	for name, l := range r.leases {
		if l.Until <= now {
			delete(r.leases, name)
			r.log.Info("lease.expire", "name", name, "holder", l.Holder)
		}
	}
}

// leasesLocked は使用中の貸し出しを名前順に返す
func (r *Room) leasesLocked() []Lease {
	r.expireLeasesLocked()
	out := make([]Lease, 0, len(r.leases))
	for _, l := range r.leases {
		out = append(out, *l)
	}
	slices.SortFunc(out, func(a, b Lease) int { return strings.Compare(a.Name, b.Name) })
	return out
}

// leaseByAgent はエージェントからの貸し出し要求を処理する。同じエージェントが借り直すと期限を延ばす
func (r *Room) leaseByAgent(token, name string, minutes int) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	t, ok := r.jobs[token]
	if !ok {
		return "", fmt.Errorf("トークンが無効です（ジョブが終了しているか、誤ったトークンです）")
	}
	name = normalizeLeaseName(name)
	if name == "" || len([]rune(name)) > 64 {
		return "", fmt.Errorf("name には1〜64文字の名前を指定してください（例: cdp-9333）")
	}
	if minutes <= 0 {
		minutes = leaseDefaultMinutes
	}
	minutes = min(minutes, leaseMaxMinutes)
	r.expireLeasesLocked()
	if l := r.leases[name]; l != nil && l.Holder != t.agentID {
		return "", fmt.Errorf("%s は @%s が使用中です（%s まで）。返却されるか期限が過ぎるまで待つか、@%s に確認してください",
			name, l.Holder, time.UnixMilli(l.Until).Format("15:04"), l.Holder)
	}
	until := time.Now().Add(time.Duration(minutes) * time.Minute)
	r.leases[name] = &Lease{Name: name, Holder: t.agentID, Until: until.UnixMilli()}
	r.log.Info("lease.acquire", "name", name, "holder", t.agentID, "minutes", minutes)
	r.pushStatusLocked()
	return fmt.Sprintf("%s を %s まで借りました。使い終わったら %s で返してください。", name, until.Format("15:04"), releaseToolName), nil
}

// releaseByAgent は借りているものを返す。借りていないものは返せない
func (r *Room) releaseByAgent(token, name string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	t, ok := r.jobs[token]
	if !ok {
		return "", fmt.Errorf("トークンが無効です（ジョブが終了しているか、誤ったトークンです）")
	}
	name = normalizeLeaseName(name)
	r.expireLeasesLocked()
	l := r.leases[name]
	if l == nil {
		return fmt.Sprintf("%s は貸し出されていません（期限切れで返却済みの可能性があります）。", name), nil
	}
	if l.Holder != t.agentID {
		return "", fmt.Errorf("%s は @%s が借りています。自分が借りたものだけ返せます", name, l.Holder)
	}
	delete(r.leases, name)
	r.log.Info("lease.release", "name", name, "holder", t.agentID)
	r.pushStatusLocked()
	return fmt.Sprintf("%s を返しました。", name), nil
}

// writeLeases は依頼文に、使用中の共有物と借り方を添える
func (r *Room) writeLeases(b *strings.Builder, token string) {
	ls := r.leasesLocked()
	if len(ls) == 0 {
		b.WriteString("- 共有物の貸し出し: 使用中のものはありません。")
	} else {
		parts := make([]string, len(ls))
		for i, l := range ls {
			parts[i] = fmt.Sprintf("%s（@%s、%s まで）", l.Name, l.Holder, time.UnixMilli(l.Until).Format("15:04"))
		}
		fmt.Fprintf(b, "- 共有物の貸し出し: 使用中 %s。", strings.Join(parts, "、"))
	}
	fmt.Fprintf(b, "CDP のポートなど共有しているものを使う前に AI Agent Room の %s ツール（token=%q、name=例 cdp-9333）で借り、使い終わったら %s で返してください。\n",
		leaseToolName, token, releaseToolName)
}

// leaseToolDefs は MCP の tools/list に載せる貸し出しツールの定義
func leaseToolDefs() []any {
	tokenProp := map[string]any{"type": "string", "description": "依頼文で渡されたジョブ用トークン"}
	nameProp := map[string]any{"type": "string", "description": "共有しているものの名前（例: cdp-9333）"}
	return []any{
		map[string]any{
			"name": leaseToolName,
			"description": "AI Agent Room グループチャットで、ほかのエージェントと共有しているもの（CDP のポートなど）を使う前に借ります。" +
				"ほかのエージェントが使用中なら失敗します。自分が借り直すと期限を延ばします。",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"token":   tokenProp,
					"name":    nameProp,
					"minutes": map[string]any{"type": "integer", "description": fmt.Sprintf("借りる時間（分）。省略時は%d分、最大%d分", leaseDefaultMinutes, leaseMaxMinutes)},
				},
				"required": []string{"token", "name"},
			},
		},
		map[string]any{
			"name":        releaseToolName,
			"description": "AI Agent Room グループチャットで、" + leaseToolName + " で借りたものを返します。",
			"inputSchema": map[string]any{
				"type":       "object",
				"properties": map[string]any{"token": tokenProp, "name": nameProp},
				"required":   []string{"token", "name"},
			},
		},
	}
}
