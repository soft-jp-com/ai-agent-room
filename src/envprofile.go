package main

// 人間の環境のプロフィールと、この会話でのルールを、会話の先頭にシステムの発言として載せる（修正案 7.3）。
// 起動時と、新しい会話を始めたとき（「新しい会話」「要約して新しい会話」、作業ディレクトリの変更）に投稿する。
// 環境変数の値は載せない（キーや秘密が混ざるため）。載せるのは OS とシェルの有無・バージョンだけ。

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// defaultRules はこの会話でのルール（rules.md がないときに使う。@claude2 の文面、#1592）
const defaultRules = `1. 人間の質問には、進行役（または名指しされた人）が先に答える。ほかの人は、訂正や反対があるときだけ発言し、同意だけなら [pass] にする。
2. 作業は、進行役が割り振ってから始める。名乗り出たら、進行役の返事を待つ。
3. 人間に実行してもらうコマンドは、進行役だけが出す。ほかの人は進行役に提案する。
4. そのコマンドは、言語指定つきのコードブロック（powershell / cmd / bash）で書く。人間は［実行］ボタンで実行できるので、スクリプトファイルは作らない。
5. コマンドは、上の「環境」にあるシェルに合わせて書く。cmd と PowerShell の書き方を混ぜない（例：cmd では $env:TEMP は使えない）。
6. 本番の変更、IAM、削除は、人間が実行する。エージェントは自分の許可ルールを書き換えず、安全確認も回避しない。ルールや権限の設定（.claude/、CLAUDE.md、GEMINI.md、rules.md など）は編集しない。
7. 秘密情報（キー、トークン、パスワード）はチャットに書かない。{{ai_agent_room_dir}} は読まない・編集しない。
8. 共有しているもの（CDP のポート、プロジェクトの logs/ など）は、使用中でないか確かめてから使う。テストのログは一時フォルダに出す。
9. 完了を報告するときは、確認した内容（build、test、画面）と、確認していないことを分けて書く。`

// defaultRulesEn は defaultRules の英語版（言語の設定が英語で、rules.en.md も rules.md もないときに使う）
const defaultRulesEn = `1. The leader (or whoever is named) answers the human's questions first. Others speak only to correct or disagree; if you only agree, reply [pass].
2. Start work only after the leader assigns it. If you volunteer, wait for the leader's reply.
3. Only the leader gives commands for the human to run. Others suggest them to the leader.
4. Write those commands in code blocks with a language (powershell / cmd / bash). The human can run them with the [Run] button, so do not create script files.
5. Write commands for a shell listed under "Environment" above. Do not mix cmd and PowerShell syntax (for example, $env:TEMP does not work in cmd).
6. The human makes production changes, IAM changes and deletions. Agents do not rewrite their own permission rules or bypass safety checks. Do not edit rules or permission settings (.claude/, CLAUDE.md, GEMINI.md, rules.md and so on).
7. Do not write secrets (keys, tokens, passwords) in the chat. Do not read or edit {{ai_agent_room_dir}}.
8. Before using something shared (CDP ports, the project's logs/ and so on), check that it is not in use. Write test logs to a temporary folder.
9. When reporting that you are done, separate what you checked (build, test, screen) from what you did not check.`

// rulesFileName はルールを差し替えるファイル。設定フォルダ（AI Agent Room のフォルダの config）に置く（人間が再ビルドせずに直せる）。
// 文面の {{ai_agent_room_dir}} は、投稿するときにこの環境の AI Agent Room のフォルダに置き換える。
// 言語の設定が英語のときは rulesFileNameEn を先に探し、なければ rulesFileName を使う（人間が書いたルールを言語の設定で無視しない）
const (
	rulesFileName   = "rules.md"
	rulesFileNameEn = "rules.en.md"
)

// exeDir は実行ファイルのフォルダ（設定ファイルの旧い置き場所）
func exeDir() string {
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	return filepath.Dir(exe)
}

// shellProbe は検出するシェル1つ
type shellProbe struct {
	name string   // 表示名
	exe  []string // 実行ファイルの候補（最初に見つかったものを使う）
	args []string // バージョンを出す引数
}

// envLookPath / envRunVersion はテストで差し替える
var (
	envLookPath   = exec.LookPath
	envRunVersion = func(exe string, args []string) (string, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, exe, args...)
		hideWindow(cmd)
		out, err := cmd.Output()
		return strings.TrimSpace(string(out)), err
	}
)

func shellProbes() []shellProbe {
	if runtime.GOOS != "windows" {
		return []shellProbe{
			{"bash", []string{"bash"}, []string{"--version"}},
			{"pwsh", []string{"pwsh"}, []string{"-NoProfile", "-Command", "$PSVersionTable.PSVersion.ToString()"}},
		}
	}
	gitBash := []string{}
	for _, d := range []string{os.Getenv("ProgramFiles"), os.Getenv("ProgramW6432")} {
		if d != "" {
			gitBash = append(gitBash, filepath.Join(d, "Git", "bin", "bash.exe"))
		}
	}
	return []shellProbe{
		{"PowerShell 7 (pwsh)", []string{"pwsh"}, []string{"-NoProfile", "-NonInteractive", "-Command", "$PSVersionTable.PSVersion.ToString()"}},
		{"Windows PowerShell", []string{"powershell"}, []string{"-NoProfile", "-NonInteractive", "-Command", "$PSVersionTable.PSVersion.ToString()"}},
		{"cmd", []string{"cmd"}, []string{"/d", "/c", "ver"}},
		{"Git Bash", gitBash, []string{"--version"}},
	}
}

// envInfo は起動時に調べた環境。文面は投稿するときに、言語の設定に合わせて text で作る
type envInfo struct {
	OS     string // GOOS/GOARCH
	Shells []shellFound
}

// shellFound はシェル1つを調べた結果
type shellFound struct {
	Name      string
	Found     bool
	VersionNG bool   // 見つかったが、バージョンを取得できなかった
	Version   string // バージョンの最初の空でない行（出力が空なら空）
}

// text は環境の部分の文面を lang（"ja" / "en"）で返す
func (e *envInfo) text(lang string) string {
	en := lang == langEn
	var b strings.Builder
	fmt.Fprintf(&b, "- OS: %s", e.OS)
	b.WriteString(pick(en, "\n- Shells:", "\n- シェル:"))
	for _, s := range e.Shells {
		v := s.Version
		switch {
		case !s.Found:
			v = pick(en, "not found", "見つかりません")
		case s.VersionNG:
			v = pick(en, "available (version unknown)", "あり（バージョンを取得できません）")
		case v == "":
			v = pick(en, "available", "あり")
		}
		fmt.Fprintf(&b, "\n  - %s: %s", s.Name, v)
	}
	return b.String()
}

// pick は en なら e を、そうでなければ j を返す
func pick(en bool, e, j string) string {
	if en {
		return e
	}
	return j
}

// detectEnvironment は OS と使えるシェルを調べる。
// 起動時に1回だけ呼ぶ（pwsh の起動に1秒ほどかかるため）。シェルが見つからなくても失敗しない
func detectEnvironment() *envInfo {
	e := &envInfo{OS: runtime.GOOS + "/" + runtime.GOARCH}
	for _, p := range shellProbes() {
		e.Shells = append(e.Shells, probeShell(p))
	}
	return e
}

func probeShell(p shellProbe) shellFound {
	for _, c := range p.exe {
		exe, err := envLookPath(c) // フルパス（Git Bash）はそのファイルがあるかを確かめる
		if err != nil {
			continue
		}
		out, err := envRunVersion(exe, p.args)
		if err != nil {
			return shellFound{Name: p.name, Found: true, VersionNG: true}
		}
		// 最初の空でない行だけを使う（cmd の ver は先頭が空行、bash --version は複数行）
		for _, line := range strings.Split(out, "\n") {
			if line = strings.TrimSpace(line); line != "" {
				return shellFound{Name: p.name, Found: true, Version: line}
			}
		}
		return shellFound{Name: p.name, Found: true}
	}
	return shellFound{Name: p.name}
}

// SetEnvProfile は起動時に調べた環境と設定フォルダ（configDir。旧い置き場所 legacyDir も読む）を設定し、
// 今の会話にプロフィールを投稿する
func (r *Room) SetEnvProfile(env *envInfo, configDir, legacyDir string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.envProfile, r.configDir, r.legacyConfigDir = env, configDir, legacyDir
	r.postProfileLocked()
}

// ConfigDir は人間が編集する設定のフォルダを返す
func (r *Room) ConfigDir() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.configDir
}

// postProfileLocked は環境のプロフィールとルールを投稿する（環境を調べる前は何もしない）。
// ルールの5番が「上の『環境』」を指すので、環境を先に並べる。文面は言語の設定（r.lang）に合わせる
func (r *Room) postProfileLocked() {
	if r.envProfile == nil {
		return
	}
	en := r.langLocked() == langEn
	rules := pick(en, defaultRulesEn, defaultRules)
	var legacy []string // 旧い置き場所から読んだファイル（人間に移してもらう）
	names := []string{rulesFileName}
	if en {
		names = []string{rulesFileNameEn, rulesFileName}
	}
	for _, name := range names {
		p, old := resolveConfigFile(r.configDir, r.legacyConfigDir, name)
		if p == "" {
			continue
		}
		if b, err := os.ReadFile(p); err == nil && strings.TrimSpace(string(b)) != "" {
			rules = strings.TrimSpace(string(b))
		}
		if old {
			legacy = append(legacy, p)
		}
		break
	}
	// ルールの文面の {{ai_agent_room_dir}} は、この環境の AI Agent Room のフォルダ（エージェントに読ませない場所）に置き換える
	rules = strings.ReplaceAll(rules, appDirPlaceholder, firstNonEmpty(r.appDirLocked(), pick(en, "the AI Agent Room folder", "AI Agent Room のフォルダ")))
	caps, oldCaps := r.capabilitiesTextLocked(en)
	if oldCaps != "" {
		legacy = append(legacy, oldCaps)
	}
	format := "【環境とこの会話でのルール】（AI Agent Room が起動時と新しい会話の開始時に投稿）\n\n■ 環境\n%s\n- 作業ディレクトリ: %s\n\n■ エージェント（できること。人間が設定フォルダの %s で宣言）\n%s\n\n■ この会話でのルール\n%s"
	if en {
		format = "[Environment and rules for this chat] (posted by AI Agent Room at startup and when a new chat starts)\n\n■ Environment\n%s\n- Working directory: %s\n\n■ Agents (what each may do, declared by the human in %s in the config folder)\n%s\n\n■ Rules for this chat\n%s"
	}
	text := fmt.Sprintf(format, r.envProfile.text(r.langLocked()), r.workdir, capabilitiesFileName, caps, rules)
	if len(legacy) > 0 {
		// 旧い置き場所はエージェントが書き換えられる場所なので、人間に移してもらう
		text += fmt.Sprintf(pick(en, "\n\n[Note] These files were read from the old location. Move them to the config folder (%s): %s",
			"\n\n【注意】次のファイルを旧い置き場所から読みました。設定フォルダ（%s）に移してください: %s"),
			r.configDir, strings.Join(legacy, pick(en, ", ", "、")))
		r.log.Warn("config.legacy", "files", strings.Join(legacy, ","))
	}
	r.postLocked("system", text, "system")
}

// appDirPlaceholder はルールの文面で、AI Agent Room のフォルダ（設定フォルダの親）に置き換える文字列
const appDirPlaceholder = "{{ai_agent_room_dir}}"

// appDirLocked は AI Agent Room のフォルダ（認証トークンと設定の置き場所）を返す
func (r *Room) appDirLocked() string {
	if r.configDir == "" {
		return ""
	}
	return filepath.Dir(r.configDir)
}

// ---- エージェントごとのできること（修正案 7.3） -------------------------------------------------

// capabilitiesFileName は、エージェントごとにできることを人間が宣言するファイル。rules.md と同じフォルダに置く。
// 例: {"claude": {"write": true, "exec": true, "prod": false}, "agy": {"write": false}}
// 書いていない項目・エージェントは「不明」と出す（AI Agent Room には実際の権限を調べる方法がないため）
const capabilitiesFileName = "capabilities.json"

// AgentCapabilities はエージェント1人のできること。nil は宣言なし（不明）
type AgentCapabilities struct {
	Write *bool `json:"write"` // ファイルを書き込めるか
	Exec  *bool `json:"exec"`  // コマンドを実行できるか
	Prod  *bool `json:"prod"`  // 本番の変更（配備・IAM・削除）をしてよいか
}

func capText(v *bool, en bool) string {
	switch {
	case v == nil:
		return pick(en, "unknown", "不明")
	case *v:
		return pick(en, "yes", "可")
	default:
		return pick(en, "no", "不可")
	}
}

// capabilitiesTextLocked は参加中のエージェントのできることを1人1行で返す（en なら英語）。旧い置き場所から読んだらそのパスも返す
func (r *Room) capabilitiesTextLocked(en bool) (text, legacyPath string) {
	caps := map[string]AgentCapabilities{}
	if p, old := resolveConfigFile(r.configDir, r.legacyConfigDir, capabilitiesFileName); p != "" {
		if old {
			legacyPath = p
		}
		if b, err := os.ReadFile(p); err == nil {
			if err := json.Unmarshal(b, &caps); err != nil {
				r.log.Warn("capabilities.load", "error", err.Error())
			}
		}
	}
	format := pick(en, "- @%s (%s): write %s / run commands %s / production %s", "- @%s（%s）: 書き込み %s / コマンド実行 %s / 本番操作 %s")
	var lines []string
	for _, a := range r.agents {
		c := caps[a.ID]
		lines = append(lines, fmt.Sprintf(format, a.ID, a.Name, capText(c.Write, en), capText(c.Exec, en), capText(c.Prod, en)))
	}
	return strings.Join(lines, "\n"), legacyPath
}
