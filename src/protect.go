package main

// エージェントから守るファイルと、その守り方（#1643〜#1649）。環境に依存するパスはコードに書かず、起動時に決める。
//
// A. 人間が編集する設定（rules.md、capabilities.json、protected.json、AI Agent Room の設定）は、
//    AI Agent Room のフォルダ（defaultSecretDir。既定は OS ごとの標準の場所）の config に置く。
//    旧い置き場所（実行ファイルのフォルダ）は、config にファイルがないときだけ読み、画面に警告を出す（移行が済んだら削除する）。
// B. 禁止ルールを渡せる CLI（Claude Code）には、起動のたびに --settings で禁止ルールを渡す。
//    - AI Agent Room のフォルダ: 読み書きとも禁止（認証トークン・設定・非公開の実行ログ）
//    - 作業ディレクトリの .claude/・CLAUDE.md・GEMINI.md・AGENTS.md、ユーザーの Claude Code の設定、start.bat: 編集を禁止
//      （hooks などで承認を通らずにコマンドを動かす経路、エージェントへの指示を書き換える経路を塞ぐ）
//    - 人間が config/protected.json に足したパス: 編集を禁止
//    許可を足す仕組みは持たない（エージェントが自分の許可を増やす経路を作らないため）。
// C. 禁止ルールを渡せない CLI（Codex、agy など）の発言と、保護するパスを含むコードブロックは、
//    画面で［実行］の前に必ず確認を出す（agents[].enforced、blocks[].protected）。
//
// 限界: パターンでの禁止はコマンドの文字列を照合するだけで、変数で組み立てたパスや別の言語からの読み込みはすり抜ける。
// AI Agent Room 自身のソースはエージェントが開発するので書き換えを禁止できない（start.bat がビルド前に変更の一覧を出して確認する）。

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// configDirName は AI Agent Room のフォルダの下の、人間が編集する設定のフォルダ
const configDirName = "config"

// protectedFileName は、人間が保護するパスを足すファイル（config に置く）。例: {"paths": ["D:\\secrets", "~/.aws"]}
const protectedFileName = "protected.json"

// defaultConfigDir は人間が編集する設定のフォルダを返す
func defaultConfigDir() string {
	d, err := defaultSecretDir()
	if err != nil {
		return ""
	}
	return filepath.Join(d, configDirName)
}

// resolveConfigFile は設定ファイル name の読み込み先と、旧い置き場所から読んだかを返す。
// config にあればそちらだけを読み、旧い置き場所（実行ファイルのフォルダ）は config にないときだけ読む
func resolveConfigFile(configDir, legacyDir, name string) (path string, legacy bool) {
	if configDir != "" {
		if p := filepath.Join(configDir, name); isFile(p) {
			return p, false
		}
	}
	if legacyDir != "" {
		if p := filepath.Join(legacyDir, name); isFile(p) {
			return p, true
		}
	}
	return "", false
}

func isFile(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

// Protection は起動時に決めた保護の内容
type Protection struct {
	SecretDir string   // 読み書きとも禁止（AI Agent Room のフォルダ）
	EditOnly  []string // 編集を禁止するファイル・フォルダ（フォルダは配下すべて）
}

// buildProtection は保護するパスを集める。workdir は会話の作業ディレクトリ、legacyDir は設定ファイルの旧い置き場所
func buildProtection(secretDir, workdir, legacyDir string) Protection {
	p := Protection{SecretDir: secretDir}
	add := func(path string) {
		if path != "" {
			p.EditOnly = append(p.EditOnly, filepath.Clean(path))
		}
	}
	if workdir != "" {
		add(filepath.Join(workdir, ".claude"))
		for _, name := range []string{"CLAUDE.md", "GEMINI.md", "AGENTS.md"} {
			add(filepath.Join(workdir, name))
		}
	}
	if home, err := os.UserHomeDir(); err == nil {
		add(filepath.Join(home, ".claude", "settings.json"))
		add(filepath.Join(home, ".claude", "settings.local.json"))
	}
	if legacyDir != "" {
		for _, name := range []string{rulesFileName, capabilitiesFileName, "start.bat"} {
			add(filepath.Join(legacyDir, name))
		}
	}
	if secretDir != "" {
		for _, extra := range loadExtraProtected(filepath.Join(secretDir, configDirName, protectedFileName)) {
			add(extra)
		}
	}
	return p
}

// loadExtraProtected は config/protected.json の paths を読む（~ はホームフォルダ）
func loadExtraProtected(path string) []string {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var v struct {
		Paths []string `json:"paths"`
	}
	if json.Unmarshal(b, &v) != nil {
		return nil
	}
	home, _ := os.UserHomeDir()
	var out []string
	for _, p := range v.Paths {
		p = strings.TrimSpace(p)
		if strings.HasPrefix(p, "~") && home != "" {
			p = filepath.Join(home, p[1:])
		}
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// claudeRulePath は OS のパスを Claude Code の権限ルールの書き方（絶対パスは //c/Users/... や //home/...）にする
func claudeRulePath(p string) string {
	p = filepath.ToSlash(filepath.Clean(p))
	if len(p) >= 2 && p[1] == ':' {
		return "//" + strings.ToLower(p[:1]) + p[2:]
	}
	if strings.HasPrefix(p, "/") {
		return "/" + p
	}
	return p
}

// DenyRules は Claude Code の禁止ルールを返す
func (p Protection) DenyRules() []string {
	var rules []string
	if p.SecretDir != "" {
		sp := claudeRulePath(p.SecretDir)
		rules = append(rules, "Read("+sp+"/**)", "Edit("+sp+"/**)")
		// コマンドで読む場合（type、cat、Get-Content など）。パスの末尾2段（例: Local/ai-agent-room）で見る
		tail := filepath.Base(filepath.Dir(p.SecretDir)) + "/" + filepath.Base(p.SecretDir)
		for _, tool := range []string{"Bash", "PowerShell"} {
			rules = append(rules, tool+"(*"+tail+"*)", tool+"(*"+strings.ReplaceAll(tail, "/", `\`)+"*)")
		}
	}
	for _, e := range p.EditOnly {
		rp := claudeRulePath(e)
		rules = append(rules, "Edit("+rp+")", "Edit("+rp+"/**)")
	}
	return rules
}

// Markers はコードブロックが保護するパスに触れるかを見分ける文字列（小文字・区切りは /）
func (p Protection) Markers() []string {
	seen := map[string]bool{}
	var out []string
	add := func(s string) {
		s = strings.ToLower(filepath.ToSlash(s))
		if s != "" && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	if p.SecretDir != "" {
		add(p.SecretDir)
		add(filepath.Base(filepath.Dir(p.SecretDir)) + "/" + filepath.Base(p.SecretDir))
	}
	for _, e := range p.EditOnly {
		add(e)
		add(filepath.Base(e)) // 相対パスや別の書き方でも気づけるよう、名前でも見る（例: .claude、start.bat）
	}
	return out
}

// ---- 起動時に決めて、各所で使う ----------------------------------------------------------------

var (
	// protection は起動時に決めた保護の内容（main で設定）。空ならエージェントに禁止ルールを渡さない
	protection Protection
	// protectMarkers は protection.Markers() の結果（コードブロックの判定に使う）
	protectMarkers []string
)

// setProtection は保護の内容を設定する
func setProtection(p Protection) {
	protection, protectMarkers = p, p.Markers()
}

// touchesProtected はコードブロックの本文が保護するパスに触れるかを返す
func touchesProtected(code string) bool {
	s := strings.ToLower(strings.ReplaceAll(code, `\`, "/"))
	for _, list := range [][]string{commonProtectMarkers, protectMarkers} {
		for _, m := range list {
			if strings.Contains(s, m) {
				return true
			}
		}
	}
	return false
}

// commonProtectMarkers は、保護の設定の有無（起動時の失敗やテスト）にかかわらず常に見る目印。
// 環境変数で書かれたパス、エージェントへの指示・許可の設定ファイル、許可の確認を飛ばすオプション
var commonProtectMarkers = []string{
	"localappdata", "ai_agent_room_config_dir", "/ai-agent-room/config",
	"ai_chat_config_dir", "/ai_chat/config", // 改名前（AI Chat）の名前。移せずに旧いフォルダが残った場合のため
	".claude", "claude.md", "gemini.md", "agents.md", "rules.md", "capabilities.json", "protected.json", "start.bat",
	"permissions", "--dangerously-skip-permissions", "bypasspermissions",
}

// claudeDenySettings は --settings に渡す JSON を返す（禁止ルールがなければ空）。
// パスに空白が含まれても崩れないよう、--disallowedTools ではなく設定の JSON で渡す
func claudeDenySettings() string {
	rules := protection.DenyRules()
	if len(rules) == 0 {
		return ""
	}
	b, _ := json.Marshal(map[string]any{"permissions": map[string]any{"deny": rules}})
	return string(b)
}

// DenyEnforcer は、起動時に禁止ルールを渡せる CLI のアダプタが実装する
type DenyEnforcer interface{ EnforcesDeny() bool }

func (a *claudeAdapter) EnforcesDeny() bool { return claudeDenySettings() != "" }

// enforcesDeny はエージェントに禁止ルールを渡せるかを返す（画面は、渡せないエージェントの［実行］に必ず確認を出す）
func enforcesDeny(a *Agent) bool {
	e, ok := a.Adapter.(DenyEnforcer)
	return ok && e.EnforcesDeny()
}

// openConfigDir は設定フォルダを作ってファイルマネージャーで開く（人間が画面のボタンから使う）
func openConfigDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("explorer", dir)
	case "darwin":
		cmd = exec.Command("open", dir)
	default:
		cmd = exec.Command("xdg-open", dir)
	}
	return cmd.Start() // explorer は成功しても終了コード 1 を返すことがあるので、終了は待たない
}

// protectBase は保護の内容を作り直すときの元（main で設定）。作業ディレクトリを変えたら作り直す
var protectBase struct{ secretDir, legacyDir string }

// initProtection は起動時に保護の内容を決める
func initProtection(secretDir, legacyDir, workdir string) {
	protectBase.secretDir, protectBase.legacyDir = secretDir, legacyDir
	setProtection(buildProtection(secretDir, workdir, legacyDir))
}

// refreshProtection は作業ディレクトリの変更に合わせて保護の内容を作り直す（起動前・テストでは何もしない）
func refreshProtection(workdir string) {
	if protectBase.secretDir == "" && protectBase.legacyDir == "" {
		return
	}
	setProtection(buildProtection(protectBase.secretDir, workdir, protectBase.legacyDir))
}
