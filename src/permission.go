package main

// エージェントの権限の段階（案8）。人間が画面で選び、設定フォルダに保存する（エージェントからは書き換えられない場所）。
// 段階は「既定」（今までどおり。CLI の設定と *_ARGS のまま）「読み取りのみ」「作業フォルダに書き込み可」の3つ。
// 「制限なし」は、他のエージェントの発言に影響されて危険な操作をするおそれがあるため用意しない。
// 既定以外を選んだときは、*_ARGS の権限のオプションを外して画面の選択を優先する。
// 守るファイルへの禁止ルール（Claude の --settings の deny）は段階にかかわらず外さない（アダプタが別に付ける）。
// Antigravity は権限の指定方法を確かめていないため非対応。

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

const (
	permDefault   = ""                // 既定（今までどおり）
	permReadOnly  = "read_only"       // 読み取りのみ
	permWorkspace = "workspace_write" // 作業フォルダに書き込み可
)

var ErrInvalidPermission = errors.New("権限の段階が不正か、このエージェントでは選べません")

// permLabel は段階の表示名（チャットの通知用）
var permLabel = map[string]string{permDefault: "既定", permReadOnly: "読み取りのみ", permWorkspace: "作業フォルダに書き込み可"}

// PermissionSetter は、権限の段階を起動時の引数で指定できる CLI のアダプタが実装する
type PermissionSetter interface {
	// PermissionArgs は段階 level（既定以外）の引数を返す
	PermissionArgs(level string) []string
	// StripPermissionArgs は *_ARGS から権限のオプションを外し、残したものと外したものを返す
	StripPermissionArgs(args []string) (kept, removed []string)
}

// Claude: 読み取りのみは、書き込みとコマンド実行のツールを禁止する（禁止は許可より優先される）。
// 利用者側の MCP（termio など）からもシェルを操作できるため、--strict-mcp-config で AI Agent Room が渡す MCP の設定だけを使わせる
func (a *claudeAdapter) PermissionArgs(level string) []string {
	switch level {
	case permReadOnly:
		return []string{"--disallowedTools=Edit,Write,NotebookEdit,Bash,PowerShell", "--strict-mcp-config"}
	case permWorkspace:
		return []string{"--permission-mode", "acceptEdits"}
	}
	return nil
}

func (a *claudeAdapter) StripPermissionArgs(args []string) (kept, removed []string) {
	return stripOptions(args, map[string]bool{"--permission-mode": true}, map[string]bool{"--dangerously-skip-permissions": true, "--allow-dangerously-skip-permissions": true}, nil)
}

// Codex: exec resume は -s を受け付けないため、設定の上書き（-c sandbox_mode=…）で指定する。
// サンドボックスは MCP サーバの側には効かないため、制限できるのはファイルの書き込みだけ（画面に注記する）
func (a *codexAdapter) PermissionArgs(level string) []string {
	switch level {
	case permReadOnly:
		return []string{"-c", `sandbox_mode="read-only"`}
	case permWorkspace:
		return []string{"-c", `sandbox_mode="workspace-write"`}
	}
	return nil
}

func (a *codexAdapter) StripPermissionArgs(args []string) (kept, removed []string) {
	return stripOptions(args, map[string]bool{"-s": true, "--sandbox": true},
		map[string]bool{"--full-auto": true, "--dangerously-bypass-approvals-and-sandbox": true, "--yolo": true},
		func(key, val string) bool { // -c / --config の sandbox_mode・sandbox_permissions
			return (key == "-c" || key == "--config") && (strings.HasPrefix(val, "sandbox_mode") || strings.HasPrefix(val, "sandbox_permissions"))
		})
}

// stripOptions は args から、値を取るオプション withValue（「--x v」と「--x=v」）、値を取らないオプション flags、
// および pair（「-c v」の組）が真を返すものを外す
func stripOptions(args []string, withValue, flags map[string]bool, pair func(key, val string) bool) (kept, removed []string) {
	for i := 0; i < len(args); i++ {
		arg := args[i]
		name, _, hasEq := strings.Cut(arg, "=")
		switch {
		case flags[arg]:
			removed = append(removed, arg)
		case withValue[name] && hasEq:
			removed = append(removed, arg)
		case withValue[arg] && i+1 < len(args):
			removed = append(removed, arg, args[i+1])
			i++
		case pair != nil && i+1 < len(args) && pair(arg, args[i+1]):
			removed = append(removed, arg, args[i+1])
			i++
		case pair != nil && hasEq && pair(name, arg[len(name)+1:]):
			removed = append(removed, arg)
		default:
			kept = append(kept, arg)
		}
	}
	return kept, removed
}

// argsWithPermission は *_ARGS（env）に段階 level を反映した引数を返す。
// 既定、または段階を指定できない CLI では *_ARGS をそのまま返す
func argsWithPermission(ad any, env, level string) []string {
	args := extraArgs(env)
	ps, ok := ad.(PermissionSetter)
	if level == permDefault || !ok {
		return args
	}
	kept, _ := ps.StripPermissionArgs(args)
	return append(kept, ps.PermissionArgs(level)...)
}

type permissionKey struct{}

// withPermission は1ターンの権限の段階を ctx に付ける（Room が設定する）
func withPermission(ctx context.Context, level string) context.Context {
	return context.WithValue(ctx, permissionKey{}, level)
}

// permissionFrom は ctx に付いた段階を返す。付いていなければ既定
func permissionFrom(ctx context.Context) string {
	level, _ := ctx.Value(permissionKey{}).(string)
	return level
}

// permissionEnvOf はアダプタの追加引数の環境変数名
func permissionEnvOf(ad any) string {
	switch ad.(type) {
	case *claudeAdapter:
		return "CLAUDE_ARGS"
	case *codexAdapter:
		return "CODEX_ARGS"
	case *agyAdapter:
		return "AGY_ARGS"
	}
	return ""
}

// permissionOverridden は、段階を選んだことで *_ARGS から外すオプションを返す（画面の表示とログ用）
func permissionOverridden(a *Agent) []string {
	ps, ok := a.Adapter.(PermissionSetter)
	if a.permission == permDefault || !ok {
		return nil
	}
	_, removed := ps.StripPermissionArgs(extraArgs(permissionEnvOf(a.Adapter)))
	return removed
}

// validPermission は、エージェント a で段階 level を選べるかを返す
func validPermission(a *Agent, level string) bool {
	if level == permDefault {
		return true
	}
	_, ok := a.Adapter.(PermissionSetter)
	return ok && (level == permReadOnly || level == permWorkspace)
}

// SetAgentPermission は人間が選んだ権限の段階を設定し、保存する（次のターンから反映）。
// 確認は画面が出す（上げる操作）。変更はログとチャットに残し、エージェント本人にも伝わるようにする
func (r *Room) SetAgentPermission(id, level string) error {
	r.mu.Lock()
	a := r.agent(id)
	if a == nil {
		r.mu.Unlock()
		return ErrAgentNotFound
	}
	if !validPermission(a, level) {
		r.mu.Unlock()
		return ErrInvalidPermission
	}
	if a.permission == level {
		r.mu.Unlock()
		return nil
	}
	r.applyPermissionLocked(a, level, "human")
	r.mu.Unlock()
	r.SaveSettings()
	return nil
}

func (r *Room) applyPermissionLocked(a *Agent, level, by string) {
	from := a.permission
	a.permission = level
	log := r.log.With("agent", a.ID, "from", firstNonEmpty(from, "default"), "to", firstNonEmpty(level, "default"), "by", by)
	log.Info("agent.permission.set")
	if removed := permissionOverridden(a); len(removed) > 0 {
		log.Warn("agent.permission.override", "removed", strings.Join(removed, " "))
	}
	if by == "human" {
		r.postLocked("system", fmt.Sprintf("%s の権限を「%s」から「%s」に変更しました（次の発言から反映）。", a.Name, permLabel[from], permLabel[level]), "system")
	}
	r.pushStatusLocked()
}

// restoreAgentPermission は保存した段階を戻す（起動時。チャットには出さない）。消えたエージェントや選べない段階は戻さない
func (r *Room) restoreAgentPermission(id, level string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	a := r.agent(id)
	if a == nil || !validPermission(a, level) {
		r.log.Warn("settings.load.agent_permission", "agent", id, "level", level, "error", ErrInvalidPermission.Error())
		return
	}
	r.applyPermissionLocked(a, level, "settings")
}
