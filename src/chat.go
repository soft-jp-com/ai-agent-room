package main

// チャットルーム: メッセージ履歴、発言待ちキュー、エージェントのターン実行を管理する。
// エージェントは1人ずつ順番に発言し、前の発言を読んだうえで答える。
//
// 発言者の決め方は2通り:
//   - チャット: 人間やエージェントの @メンションで発言待ちキューに入ったエージェントが発言する。
//     1回の発言で呼ばれたエージェント（@all・宛先なしなら全員）は同時に起動し、
//     互いの回答は次のターンで新着として受け取る
//   - ディスカッション: お題に対して、参加エージェントが決まった順番で指定周回数だけ発言する
// どちらも、エージェントの発言が終わってから次のエージェントを起動するまで delay だけ待つ。
// 待機中に人間が発言すれば、その内容は次のエージェントに渡される。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"
)

// Message はチャットの1発言。チャットログ（chat-*.jsonl）にもこの形式で1行ずつ記録する。
type Message struct {
	ID    int    `json:"id"`
	Time  string `json:"time"`            // 発言時刻（ローカル時刻 "2006-01-02 15:04:05"）
	From  string `json:"from"`            // "human" / "system" / エージェントID
	Name  string `json:"name"`            // 発言者の表示名
	Model string `json:"model,omitempty"` // 発言したモデル（エージェントのみ）
	Kind  string `json:"kind"`            // chat / pass / system / command_result（コードブロックの実行結果。From は system）
	Text  string `json:"text"`
	TS    int64  `json:"ts"` // 発言時刻（UnixMilli）
	// ReadUpTo はエージェントの返答が読んでいた最後の発言ID。返答を書いている間に届いた発言
	// （ReadUpTo より後）は反映されていないので、行き違いの見分けに使う
	ReadUpTo int `json:"read_upto,omitempty"`
	// ReplyTo は人間が「返信」で投稿したときの、返信先の発言ID（案 8）。
	// command_result では、実行開始の system の発言のID（その発言の reply_to が、実行したブロックを含む発言）
	ReplyTo int `json:"reply_to,omitempty"`
	// Blocks は本文中のコードブロック（修正案 7.1）。画面はこの添字で［実行］ボタンを付け、実行 API もこの添字で本文を取り直す。
	// chat の発言だけに付ける。再起動で戻した発言からは外す（実行済みかを再起動後に判別できないため）
	Blocks []CodeBlock `json:"blocks,omitempty"`
	// Retry は、ターンの失敗を知らせるシステムメッセージに付ける、もう一度起動できるエージェントID（案 8.3）。
	// 失敗時は cursor を進めていないので、同じ新着をそのまま渡し直せる
	Retry string `json:"retry,omitempty"`
}

type Agent struct {
	ID      string
	Type    string // CLI の種類（claude / codex / agy）。既定のエージェントは ID と同じ
	Name    string
	Color   string
	Aliases []string
	Adapter Adapter

	sessionID     string     // CLI 側の会話ID（2ターン目以降は resume する）
	cursor        int        // このエージェントに渡し済みのメッセージ数
	state         string     // idle / thinking / unavailable
	since         time.Time  // thinking 開始時刻
	model         string     // 直近のターンで使われたモデル
	modelSel      string     // 画面で選ばれたモデル（空なら CLI の既定）
	humanModel    string     // 人間が画面で選んだモデル（案2）。設定に保存して再起動後に戻す。エージェントが set_my_model で変えた分は含めない
	waitUntil     time.Time  // フリートークで発言を考え始める予定時刻（state が waiting のとき）
	activity      string     // 考え中に実行しているツール（CLI の出力から取り出した途中経過）
	editing       []string   // このターンで書き込んだファイル（作業中の担当。ターンの終わりに外す）
	paused        bool       // 一時停止中（会話に参加させない）。利用枠の上限エラーで自動的に入る
	interactive   bool       // 対話モードの窓を開いている（案9。会話に参加させない。保存しない）
	permission    string     // 人間が選んだ権限の段階（案8。permission.go。空なら既定）
	usage         UsageTotal // トークン使用量の累計（logs/usage.json に保持）
	quota         Quota      // プランの残り利用枠（logs/usage.json に保持。取得できない CLI は空）
	quotaTried    time.Time  // 直近に利用枠の取得を試みた時刻
	quotaBusy     bool       // 利用枠を取得中
	modelVer      int        // モデル設定の版番号（変更のたびに増える）。古いジョブからの変更要求で上書きしないために使う
	lastInput     int        // 直近1回の実行の文脈の大きさ（最後の API 呼び出しの入力。取れない CLI は実行全体の入力。キャッシュ分を含む。取得できなければ前回の値のまま）
	sessionStart  int        // 今の CLI セッションに最初に渡した発言のID（使用量を取得できない CLI の切り替え判定に使う）
	rotatePending bool       // CLI セッションの切り替えを予約済み（次のターンの開始時に切り替える）
	timeoutSec    int        // このエージェントの1ターンの上限時間（秒）。0 なら会話全体の設定に従う（案 8.3）

	// console は開いている対話モードの窓（案9。interactive の間だけ）
	console consoleSession
}

type AgentStatus struct {
	ID         string   `json:"id"`
	Type       string   `json:"type"`
	Name       string   `json:"name"`
	Color      string   `json:"color"`
	Removable  bool     `json:"removable,omitempty"` // 追加したエージェント（削除できる）
	State      string   `json:"state"`
	Since      int64    `json:"since,omitempty"`
	HasSession bool     `json:"has_session"`
	Model      string   `json:"model,omitempty"` // 直近のターンで使われたモデル
	ModelSel   string   `json:"model_sel"`       // 選択中のモデル（空なら既定）
	WaitUntil  int64    `json:"wait_until,omitempty"`
	Activity   string   `json:"activity,omitempty"` // 考え中に実行しているツール
	Editing    []string `json:"editing,omitempty"`  // 作業中のファイル（このターンで書き込んだもの）
	Paused     bool     `json:"paused,omitempty"`   // 一時停止中
	Quota      string   `json:"quota,omitempty"`    // プランの残り利用枠の文面（取得できない CLI は含めない）
	// Usage はトークン使用量の累計（logs/usage.json と同じ値。実行の記録がなければ含めない）。LastInput は直近1回の実行の入力（案15）
	Usage     *UsageTotal `json:"usage,omitempty"`
	LastInput int         `json:"last_input,omitempty"`
	// TimeoutSec はこのエージェントだけの1ターンの上限時間（秒）。0 なら会話全体の設定に従う（案 8.3）
	TimeoutSec int `json:"timeout_sec,omitempty"`
	// Enforced は、起動時に禁止ルールを渡せる CLI か（protect.go）。false のエージェントの［実行］には画面が必ず確認を出す
	Enforced bool `json:"enforced"`
	// Interactive は対話モードの窓を開いている間 true（案9）
	Interactive bool `json:"interactive,omitempty"`
	// InteractiveOK は対話モードの窓を開けるか（CLI が対応し、窓を表示できる環境）
	InteractiveOK bool `json:"interactive_ok,omitempty"`
	// InteractiveManual は、窓が閉じたことを自動で検知できないため［対話を終了］を出すか（macOS・Linux）
	InteractiveManual bool `json:"interactive_manual,omitempty"`
	// Permission は人間が選んだ権限の段階（空なら既定）。PermissionOK は段階を選べる CLI か。
	// PermissionOverride は、段階を選んだために *_ARGS から外している権限のオプション（案8）
	Permission         string   `json:"permission,omitempty"`
	PermissionOK       bool     `json:"permission_ok,omitempty"`
	PermissionOverride []string `json:"permission_override,omitempty"`
}

// Discussion はディスカッションの進行状況
type Discussion struct {
	Topic     string   `json:"topic"`
	Order     []string `json:"order"`      // 発言順（開始時点で利用可能なエージェント）
	Round     int      `json:"round"`      // 現在の周（1始まり）
	MaxRounds int      `json:"max_rounds"` // 最大周回数
	Next      string   `json:"next"`       // 次の発言者
	idx       int      // Order 内の次の発言者の位置
	passes    int      // 現在の周でパスした人数
}

type Waiting struct {
	Agents []string `json:"agents"` // 次に起動するエージェント（同時に起動する場合は複数）
	Until  int64    `json:"until"`  // 起動予定時刻（UnixMilli）
}

type Event struct {
	Type         string        `json:"type"` // snapshot / message / status / reset / command
	Messages     []Message     `json:"messages,omitempty"`
	Message      *Message      `json:"message,omitempty"`
	Partial      *Partial      `json:"partial,omitempty"` // type=partial: 書きかけの本文（案5）。会話には入れず、画面にだけ出す
	Agents       []AgentStatus `json:"agents,omitempty"`
	Queue        []string      `json:"queue,omitempty"`
	Hops         int           `json:"hops"`
	MaxHops      int           `json:"max_hops"`
	DelaySec     int           `json:"delay_sec"`
	Leader       string        `json:"leader"` // 進行役のエージェントID（未指定は空）
	Discussion   *Discussion   `json:"discussion,omitempty"`
	FreeTalk     *FreeTalk     `json:"free_talk,omitempty"`
	Waiting      *Waiting      `json:"waiting,omitempty"`
	RotateTokens int           `json:"rotate_tokens"` // CLI セッションを切り替えるしきい値（0 なら切り替えない）
	// TurnTimeoutSec は1ターンの上限時間（秒）。エージェントごとの設定がなければこの値を使う（案 8.3）
	TurnTimeoutSec int    `json:"turn_timeout_sec"`
	Workdir        string `json:"workdir,omitempty"`
	LogFile        string `json:"log_file,omitempty"`
	// Commands はコードブロックの実行状態（実行中・実行済み）。status イベントで毎回すべてを送る（修正案 7.1）
	Commands []CommandRun `json:"commands,omitempty"`
	// Leases は共有物の貸し出し（使用中のもの）
	Leases []Lease `json:"leases,omitempty"`
	// Command は command イベントで配信する、状態が変わった1件
	Command *CommandRun `json:"command,omitempty"`
	// CommandLeaderOnly が true なら、進行役（と人間）の発言のブロックだけが実行できる
	CommandLeaderOnly bool `json:"command_leader_only"`
	// OS は AI Agent Room が動いている OS（runtime.GOOS）。Windows 以外では cmd・bat のブロックを実行できない
	OS string `json:"os"`
	// Lang は言語の設定（"ja" / "en"。未設定なら空）。画面はこれに合わせて表示言語を切り替える
	Lang string `json:"lang"`
}

type Room struct {
	mu                sync.Mutex
	agents            []*Agent
	messages          []Message
	queue             [][]string  // 発言待ち（チャット時）。要素ごとに、同時に起動するエージェントIDのグループ
	disc              *Discussion // ディスカッション中なら非nil
	free              *FreeTalk   // フリートーク中なら非nil
	cond              *sync.Cond  // 新しい発言・停止をフリートークの待機ループに知らせる（mu と組で使う）
	lastPost          time.Time   // 最後の発言時刻
	hops              int         // 直近の人間の発言以降に行われたエージェントのターン数（チャット時）
	maxHops           int
	delay             time.Duration // エージェントの発言後、次のエージェントを起動するまでの待ち時間
	leader            string        // 進行役のエージェントID（未指定は空）。「新しい会話」でも保持する
	waiting           *Waiting
	running           bool
	cancel            context.CancelFunc // 実行中のターンまたは待機を中止する
	nextID            int
	turnSeq           int
	gen               int           // Reset のたびに増える。古い会話のターン結果を捨てるために使う
	summarizing       bool          // 「要約して新しい会話」の要約を作成中
	rotateTokens      int           // CLI セッションを切り替えるしきい値（直近1回の入力トークン。0 なら切り替えない）
	turnTimeout       time.Duration // 1ターンの上限時間（エージェントごとの設定がなければこれを使う）
	minutes           *minutesJob   // 作成中の議事録（なければ nil）
	lastMinutes       *minutesJob   // 直近に作成できた議事録
	liveMu            sync.Mutex
	live              map[string]*liveLog // エージェントごとの CLI 出力（別窓表示用。r.mu ではなく liveMu で守る）
	workdir           string
	logDir            string
	logFile           string
	jobs              map[string]*jobTicket  // 実行中のジョブ（キーはジョブ用トークン）。自己管理ツールの呼び出し元の識別に使う
	commands          map[string]*CommandRun // コードブロックの実行状態（キーは "発言ID:ブロック番号"）。今の会話で実行したものすべて
	cmdRunning        map[string]*CommandRun // 実行中のコマンド（commandKey → 実行。同時に commandMaxRunning 件まで）
	commandLeaderOnly bool                   // 進行役（と人間）の発言のブロックだけを実行できる
	envProfile        *envInfo               // 起動時に調べた環境（新しい会話の先頭に載せる。nil なら載せない）
	lang              string                 // 言語の設定（"ja" / "en"。空は未設定で日本語）。環境とルールの投稿の文面に使う
	configDir         string                 // 人間が編集する設定のフォルダ（rules.md・capabilities.json。%LOCALAPPDATA%\ai-agent-room\config）
	legacyConfigDir   string                 // 設定ファイルの旧い置き場所（実行ファイルのフォルダ）。configDir になければ読む
	leases            map[string]*Lease      // 共有物の貸し出し（キーは名前）。「新しい会話」でも保持する
	clients           map[chan Event]struct{}
	log               *slog.Logger
}

func NewRoom(agents []*Agent, workdir, logDir string, maxHops int, delay time.Duration, log *slog.Logger) *Room {
	for _, a := range agents {
		a.state = "idle"
		if !a.Adapter.Available() {
			a.state = "unavailable"
		}
	}
	r := &Room{agents: agents, maxHops: normalizeMaxHops(maxHops), delay: delay, rotateTokens: defaultRotateTokens, turnTimeout: defaultTurnTimeout, nextID: 1, workdir: workdir, logDir: logDir,
		jobs: map[string]*jobTicket{}, commands: map[string]*CommandRun{}, cmdRunning: map[string]*CommandRun{}, leases: map[string]*Lease{}, clients: map[chan Event]struct{}{}, log: log}
	r.cond = sync.NewCond(&r.mu)
	r.logFile = r.newLogFile()
	r.restoreSessionLocked()
	r.saveSessionLocked() // 最初のターンが終わる前に終了しても、この会話を復元できるようにする
	r.loadUsageLocked()
	return r
}

// newLogFile は新しい会話のログファイル名を返す。同じ秒に「新しい会話」を押しても前の会話と
// 同じファイルにならないよう、現在のファイルか既存のファイルと重なる場合は1秒ずつずらす。
// 存在の確認自体に失敗した場合は探索を打ち切り、その名前を使う（書き込みの失敗は chatlog.write で記録される）。
func (r *Room) newLogFile() string {
	t := time.Now()
	for {
		path := filepath.Join(r.logDir, "chat-"+t.Format("20060102-150405")+".jsonl")
		if path != r.logFile {
			_, err := os.Stat(path)
			if errors.Is(err, os.ErrNotExist) {
				return path
			}
			if err != nil {
				r.log.Error("chatlog.name", "error", err.Error(), "path", path)
				return path
			}
		}
		t = t.Add(time.Second)
	}
}

// ---- 購読（SSE） ---------------------------------------------------------------

func (r *Room) Subscribe() (chan Event, func()) {
	ch := make(chan Event, 256)
	r.mu.Lock()
	ch <- Event{Type: "snapshot", Messages: append([]Message{}, r.messages...)}
	ch <- r.statusLocked()
	r.clients[ch] = struct{}{}
	r.mu.Unlock()
	return ch, func() {
		r.mu.Lock()
		delete(r.clients, ch)
		r.mu.Unlock()
	}
}

func (r *Room) broadcastLocked(ev Event) {
	for ch := range r.clients {
		select {
		case ch <- ev:
		default:
			// 詰まったクライアントは切断する。取りこぼしたまま続けると、その発言が再読み込みまで表示されない。
			// 切断すると画面の EventSource が再接続し、snapshot で全件を受け取り直す
			delete(r.clients, ch)
			close(ch)
			r.log.Warn("sse.client_lagged", "event", ev.Type, "buffer", cap(ch))
		}
	}
}

func (r *Room) statusLocked() Event {
	st := make([]AgentStatus, 0, len(r.agents))
	for _, a := range r.agents {
		s := AgentStatus{ID: a.ID, Type: a.Type, Removable: a.ID != a.Type, Name: a.Name, Color: a.Color, State: a.state, HasSession: a.sessionID != "", Model: a.model, ModelSel: a.modelSel, Activity: a.activity, Editing: slices.Clone(a.editing), Paused: a.paused, Enforced: enforcesDeny(a), TimeoutSec: a.timeoutSec}
		if _, ok := a.Adapter.(QuotaFetcher); ok {
			s.Quota = a.quota.summary()
		}
		if a.usage.Jobs > 0 {
			u := a.usage
			s.Usage = &u
		}
		s.LastInput = a.lastInput
		s.Interactive = a.interactive
		s.InteractiveManual = a.interactive && a.console != nil && !a.console.AutoDetect()
		s.Permission, s.PermissionOverride = a.permission, permissionOverridden(a)
		_, s.PermissionOK = a.Adapter.(PermissionSetter)
		if _, ok := a.Adapter.(InteractiveStarter); ok {
			s.InteractiveOK = interactiveOK
		}
		if !a.since.IsZero() {
			s.Since = a.since.UnixMilli()
		}
		if !a.waitUntil.IsZero() {
			s.WaitUntil = a.waitUntil.UnixMilli()
		}
		st = append(st, s)
	}
	ev := Event{Type: "status", Agents: st, Queue: r.queuedIDsLocked(),
		Hops: r.hops, MaxHops: r.maxHops, DelaySec: int(r.delay / time.Second), Leader: r.leader,
		Waiting: r.waiting, Workdir: r.workdir, LogFile: filepath.Base(r.logFile), RotateTokens: r.rotateTokens,
		TurnTimeoutSec: int(r.turnTimeout / time.Second),
		Commands:       r.commandsLocked(), CommandLeaderOnly: r.commandLeaderOnly, Leases: r.leasesLocked(), OS: runtime.GOOS, Lang: r.lang}
	if r.disc != nil {
		d := *r.disc
		ev.Discussion = &d
	}
	if r.free != nil {
		f := *r.free
		ev.FreeTalk = &f
	}
	return ev
}

func (r *Room) pushStatusLocked() { r.broadcastLocked(r.statusLocked()) }

func (r *Room) postLocked(from, text, kind string) Message {
	return r.postModelLocked(from, "", text, kind, 0)
}

func (r *Room) postModelLocked(from, model, text, kind string, readUpTo int) Message {
	return r.appendMessageLocked(Message{From: from, Model: model, Kind: kind, Text: text, ReadUpTo: readUpTo})
}

// appendMessageLocked は m に ID・時刻・表示名を付けて会話に加え、ログに書いて配信する
func (r *Room) appendMessageLocked(m Message) Message {
	now := time.Now()
	from := m.From
	name := from
	switch from {
	case "human":
		name = "人間"
	case "system":
		name = "システム"
	default:
		if a := r.agent(from); a != nil {
			name = a.Name
		}
	}
	m.ID, m.Time, m.Name, m.TS = r.nextID, now.Format("2006-01-02 15:04:05"), name, now.UnixMilli()
	if m.Kind == "chat" {
		m.Blocks = extractCodeBlocks(m.Text)
	}
	r.nextID++
	r.messages = append(r.messages, m)
	if b, err := json.Marshal(m); err == nil {
		if f, err := os.OpenFile(r.logFile, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644); err == nil {
			f.Write(append(b, '\n'))
			f.Close()
		} else {
			r.log.Error("chatlog.write", "error", err.Error())
		}
	}
	r.broadcastLocked(Event{Type: "message", Message: &m})
	r.lastPost = now
	r.cond.Broadcast() // フリートークの待機ループを起こす
	return m
}

// ---- メンション --------------------------------------------------------------

var mentionRe = regexp.MustCompile(`@([A-Za-z][\w-]*|全員)`)

// mentions は text 中の @ID を解決する。@all / @全員 なら all=true。
func (r *Room) mentions(text, self string) (ids []string, all bool) {
	for _, m := range mentionRe.FindAllStringSubmatch(text, -1) {
		key := strings.ToLower(m[1])
		if key == "all" || key == "everyone" || key == "全員" {
			all = true
			continue
		}
		for _, a := range r.agents {
			if a.ID == self || contains(ids, a.ID) {
				continue
			}
			if contains(a.Aliases, key) {
				ids = append(ids, a.ID)
			}
		}
	}
	return
}

func (r *Room) activeIDs(except string) []string {
	var ids []string
	for _, a := range r.agents {
		if a.active() && a.ID != except {
			ids = append(ids, a.ID)
		}
	}
	return ids
}

func (r *Room) agent(id string) *Agent {
	for _, a := range r.agents {
		if a.ID == id {
			return a
		}
	}
	return nil
}

// enqueueLocked は ids を1つのグループとして発言待ちに入れる。グループ内のエージェントは同時に起動する。
func (r *Room) enqueueLocked(ids []string) {
	queued := r.queuedIDsLocked()
	var group []string
	for _, id := range ids {
		if a := r.agent(id); a != nil && a.active() && !contains(queued, id) && !contains(group, id) {
			group = append(group, id)
		}
	}
	if len(group) > 0 {
		r.queue = append(r.queue, group)
	}
	r.pushStatusLocked()
	r.startPumpLocked()
}

func (r *Room) queuedIDsLocked() []string {
	var ids []string
	for _, g := range r.queue {
		ids = append(ids, g...)
	}
	return ids
}

func (r *Room) startPumpLocked() {
	if !r.running && (len(r.queue) > 0 || r.disc != nil) {
		r.running = true
		go r.pump()
	}
}

// ---- 操作（HTTP ハンドラから呼ばれる） ----------------------------------------------

var (
	ErrEmptyMessage   = errors.New("メッセージが空です")
	ErrInvalidReply   = errors.New("返信先の発言がありません")
	ErrEmptyTopic     = errors.New("お題が空です")
	ErrNoAgents       = errors.New("参加できるエージェントがいません")
	ErrDiscussionBusy = errors.New("ディスカッションまたはフリートーク中です。停止してから開始してください")
	ErrAgentNotFound  = errors.New("エージェントが見つかりません")
	ErrInvalidModel   = errors.New("モデル名が不正です")
	ErrAgentNoLeader  = errors.New("参加できないエージェントは進行役にできません")
	ErrInvalidWorkdir = errors.New("作業ディレクトリが見つかりません")
)

// PostHuman は人間の発言を追加する。
// チャット時は宛先のエージェントを同時に起動する（宛先なし / @all なら全員）。
// ディスカッション中は発言順を変えず、次の発言者が新着として受け取る。
func (r *Room) PostHuman(text string) (Message, error) {
	return r.PostHumanReply(text, 0)
}

// hasMessageLocked は今の会話に発言 id があるかを返す（新しい会話・作業ディレクトリの切り替えより前の発言は含まない）
func (r *Room) hasMessageLocked(id int) bool {
	for i := len(r.messages) - 1; i >= 0; i-- {
		if r.messages[i].ID == id {
			return true
		}
	}
	return false
}

// PostHumanReply は人間の発言を投稿する。replyTo が 0 でなければ、その発言への返信として投稿する（案 8）
func (r *Room) PostHumanReply(text string, replyTo int) (Message, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return Message{}, ErrEmptyMessage
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if replyTo != 0 && !r.hasMessageLocked(replyTo) {
		return Message{}, ErrInvalidReply
	}
	m := r.appendMessageLocked(Message{From: "human", Kind: "chat", Text: text, ReplyTo: replyTo})
	r.checkRotateLocked()
	if r.free != nil {
		// フリートーク中は各エージェントが新着を見て自分から発言する。人間の発言で発言数の上限をリセットする。
		// 進行役（または名指しされた人）が先に答え、ほかのエージェントはその回答を読んでから発言する（ルール1）
		r.hops = 0
		r.setFirstRespondersLocked(text)
		r.free.limitNotified = false
		r.pushStatusLocked()
		return m, nil
	}
	if r.disc != nil {
		r.pushStatusLocked()
		return m, nil
	}
	r.hops = 0
	ids, all := r.mentions(text, "human")
	if all || len(ids) == 0 {
		ids = r.activeIDs("")
	}
	r.enqueueLocked(ids)
	return m, nil
}

// StartDiscussion はお題を投稿し、参加エージェントに順番に発言させる。
func (r *Room) StartDiscussion(topic string, rounds int) (Message, error) {
	topic = strings.TrimSpace(topic)
	if topic == "" {
		return Message{}, ErrEmptyTopic
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.disc != nil || r.free != nil {
		return Message{}, ErrDiscussionBusy
	}
	order := r.activeIDs("")
	if len(order) == 0 {
		return Message{}, ErrNoAgents
	}
	rounds = max(1, min(20, rounds))
	r.queue = nil // チャットの発言待ちはディスカッションに置き換える
	m := r.postLocked("human", topic, "chat")
	r.checkRotateLocked()
	names := make([]string, len(order))
	for i, id := range order {
		names[i] = "@" + id
	}
	r.postLocked("system", fmt.Sprintf("ディスカッション開始: %s の順に最大%d周発言します。1周の間に全員がパスしたら終了します。",
		strings.Join(names, " → "), rounds), "system")
	r.disc = &Discussion{Topic: topic, Order: order, Round: 1, MaxRounds: rounds, Next: order[0]}
	r.log.Info("discussion.start", "rounds", rounds, "order", strings.Join(order, ","))
	r.pushStatusLocked()
	r.startPumpLocked()
	return m, nil
}

func (r *Room) Stop() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.stopLocked()
	r.postLocked("system", "停止しました。", "system")
}

func (r *Room) stopLocked() {
	r.queue = nil
	if r.disc != nil {
		r.log.Info("discussion.end", "reason", "stopped", "round", r.disc.Round)
		r.disc = nil
	}
	r.endFreeTalkLocked("stopped")
	if r.cancel != nil {
		r.cancel()
	}
	clear(r.jobs) // 停止後はジョブの終了を待たずに自己管理ツールの要求を拒否する
	r.pushStatusLocked()
}

// Reset は履歴と各エージェントの会話セッションを破棄して新しい会話を始める。
func (r *Room) Reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.resetLocked()
	r.postProfileLocked()
}

func (r *Room) resetLocked() {
	r.stopLocked()
	r.cancelCommandsLocked()
	r.messages = nil
	r.gen++
	for _, a := range r.agents {
		a.sessionID, a.cursor, a.model = "", 0, ""
		a.lastInput, a.sessionStart, a.rotatePending = 0, 0, false
		a.editing = nil
	}
	r.minutes, r.lastMinutes = nil, nil // 作成中の議事録は gen の変化で捨てられる
	r.logFile = r.newLogFile()
	r.saveSessionLocked()
	r.broadcastLocked(Event{Type: "reset"})
	r.pushStatusLocked()
}

// SetMaxHops は人間の発言1回あたりのエージェントの発言数の上限を変える。0 は無制限
func (r *Room) SetMaxHops(n int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.maxHops = normalizeMaxHops(n)
	if r.free != nil {
		r.free.limitNotified = false
		r.cond.Broadcast() // 上限で止まっているフリートークを再開させる
	}
	r.pushStatusLocked()
}

// normalizeMaxHops は上限の値を整える。0 は無制限で、負の値は 0（無制限）にせず最小の 1 にする
func normalizeMaxHops(n int) int {
	if n < 0 {
		return 1
	}
	return n
}

// hopLimitReached は上限に達したか（上限 0 は無制限なので達しない）
func hopLimitReached(hops, maxHops int) bool {
	return maxHops > 0 && hops >= maxHops
}

// SetAgentModel はエージェントのモデルを変更する（空なら CLI の既定に戻す）。
// 次のターンから反映され、会話のセッションはそのまま引き継ぐ。
func (r *Room) SetAgentModel(id, model string) error {
	model = strings.TrimSpace(model)
	if model != "" && !modelIDRe.MatchString(model) {
		return ErrInvalidModel
	}
	r.mu.Lock()
	a := r.agent(id)
	if a == nil {
		r.mu.Unlock()
		return ErrAgentNotFound
	}
	saved := a.humanModel != model
	a.humanModel = model
	if a.modelSel != model {
		r.applyModelLocked(a, model, "human")
	}
	r.mu.Unlock()
	if saved { // 人間の選択だけを保存する（案2）
		r.SaveSettings()
	}
	return nil
}

// applyModelLocked はモデルの選択を変更し、版番号を進めて通知する。by は変更者（human / agent）
func (r *Room) applyModelLocked(a *Agent, model, by string) {
	a.modelSel = model
	a.modelVer++
	label := firstNonEmpty(model, "既定のモデル")
	r.log.Info("agent.model.set", "agent", a.ID, "model", model, "by", by, "model_ver", a.modelVer)
	who := ""
	if by == "agent" {
		who = "（本人の判断）"
	}
	r.postLocked("system", fmt.Sprintf("%s のモデルを %s に変更しました%s（次の発言から反映）。", a.Name, label, who), "system")
	r.pushStatusLocked()
}

// Models はエージェントごとの選べるモデルの候補を返す。
func (r *Room) Models() map[string][]ModelOption {
	res := map[string][]ModelOption{}
	for _, a := range r.agentsSnapshot() {
		res[a.ID] = a.Adapter.Models()
	}
	return res
}

func (r *Room) SetDelay(sec int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.delay = time.Duration(max(0, min(300, sec))) * time.Second
	r.pushStatusLocked()
}

// normalizeWorkdir は作業ディレクトリの指定を絶対パスにし、ディレクトリとして存在するか確かめる。
func normalizeWorkdir(path string) (string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return "", ErrInvalidWorkdir
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", ErrInvalidWorkdir
	}
	if fi, err := os.Stat(abs); err != nil || !fi.IsDir() {
		return "", ErrInvalidWorkdir
	}
	return abs, nil
}

// SetWorkdir は作業ディレクトリを変更する。各 CLI の会話は作業ディレクトリに結び付くため、
// 変更すると実行中の処理を止め、新しい会話として始める（Reset と同じ）。
func (r *Room) SetWorkdir(path string) error {
	abs, err := normalizeWorkdir(path)
	if err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if abs == r.workdir {
		return nil
	}
	r.log.Info("room.workdir.set", "workdir", abs, "previous", r.workdir)
	r.archiveWorkdirLocked() // 戻ってきたときに復元できるよう、今の会話を残す（案 10）
	r.workdir = abs
	refreshProtection(abs) // 作業ディレクトリの .claude/ などを守り直す
	r.resetLocked()
	if r.restoreWorkdirLocked() {
		r.saveSessionLocked()
		r.broadcastLocked(Event{Type: "snapshot", Messages: r.messages})
		r.postLocked("system", fmt.Sprintf("作業ディレクトリを %s に変更し、このディレクトリの前回の会話を復元しました。", abs), "system")
		r.postProfileLocked()
		r.pushStatusLocked()
		return nil
	}
	r.postLocked("system", fmt.Sprintf("作業ディレクトリを %s に変更し、新しい会話を始めました。", abs), "system")
	r.postProfileLocked()
	return nil
}

// SetLeader は進行役を変更する（空なら指定なし）。次のターンのプロンプトから反映する。
func (r *Room) SetLeader(id string) error {
	id = strings.TrimSpace(id)
	r.mu.Lock()
	defer r.mu.Unlock()
	if id != "" {
		a := r.agent(id)
		if a == nil {
			return ErrAgentNotFound
		}
		if !a.active() {
			return ErrAgentNoLeader
		}
	}
	if r.leader == id {
		return nil
	}
	r.leader = id
	r.log.Info("room.leader.set", "leader", id)
	if id == "" {
		r.postLocked("system", "進行役の指定を解除しました（次の発言から反映）。", "system")
	} else {
		r.postLocked("system", fmt.Sprintf("進行役を %s に指定しました（次の発言から反映）。", r.agent(id).Name), "system")
	}
	r.pushStatusLocked()
	return nil
}

// ---- ターン実行 ----------------------------------------------------------------

// peekNextLocked は次に起動するエージェントのグループを返す（取り出さない）。
func (r *Room) peekNextLocked() ([]string, bool) {
	if r.disc != nil {
		return []string{r.disc.Order[r.disc.idx]}, true
	}
	if len(r.queue) > 0 {
		return r.queue[0], true
	}
	return nil, false
}

func (r *Room) pump() {
	ranTurn := false // 直前にエージェントのターンを実行したか（次の起動前に待つ）
	for {
		r.mu.Lock()
		next, ok := r.peekNextLocked()
		if !ok {
			r.running = false
			r.pushStatusLocked()
			r.mu.Unlock()
			return
		}
		if r.disc == nil && hopLimitReached(r.hops, r.maxHops) {
			r.queue = nil
			r.running = false
			r.postLocked("system", fmt.Sprintf("自動ターンの上限（%d）に達したので停止しました。続けるにはメッセージを送ってください。", r.maxHops), "system")
			r.pushStatusLocked()
			r.mu.Unlock()
			return
		}
		if ranTurn && r.delay > 0 {
			// 待機。停止されたら中断し、状態を読み直す
			ctx, cancel := context.WithCancel(context.Background())
			r.cancel = cancel
			r.waiting = &Waiting{Agents: next, Until: time.Now().Add(r.delay).UnixMilli()}
			delay := r.delay
			r.pushStatusLocked()
			r.mu.Unlock()
			select {
			case <-time.After(delay):
			case <-ctx.Done():
			}
			cancel()
			r.mu.Lock()
			r.waiting, r.cancel = nil, nil
			r.mu.Unlock()
			ranTurn = false
			continue
		}
		if r.disc == nil {
			r.queue = r.queue[1:]
		}
		// グループ内のエージェントを同時に起動する。停止時は r.cancel でまとめて中止する
		ctx, cancel := context.WithCancel(context.Background())
		r.cancel = cancel
		r.mu.Unlock()

		outcomes := make([]turnOutcome, len(next))
		var wg sync.WaitGroup
		for i, id := range next {
			wg.Add(1)
			go func() {
				defer wg.Done()
				outcomes[i] = r.runTurn(ctx, r.agent(id))
			}()
		}
		wg.Wait()
		cancel()

		r.mu.Lock()
		r.cancel = nil
		ranTurn = false
		for _, o := range outcomes {
			ranTurn = ranTurn || o != outcomeSkipped
		}
		if len(next) == 1 {
			r.advanceDiscussionLocked(next[0], outcomes[0])
		}
		r.mu.Unlock()
	}
}

type turnOutcome int

const (
	outcomeSpoke turnOutcome = iota
	outcomePass
	outcomeFailed
	outcomeSkipped  // 新着がなかったのでCLIを起動しなかった
	outcomeCanceled // 停止またはリセットされた
)

// advanceDiscussionLocked はディスカッションの発言順を1つ進め、終了条件を判定する。
func (r *Room) advanceDiscussionLocked(id string, outcome turnOutcome) {
	d := r.disc
	if d == nil || outcome == outcomeCanceled || d.Order[d.idx] != id {
		return
	}
	if outcome == outcomePass || outcome == outcomeSkipped {
		d.passes++
	}
	d.idx++
	if d.idx < len(d.Order) {
		d.Next = d.Order[d.idx]
		r.pushStatusLocked()
		return
	}
	switch {
	case d.passes == len(d.Order):
		r.endDiscussionLocked("全員がパスしたので、ディスカッションを終了しました。", "all_passed")
	case d.Round >= d.MaxRounds:
		r.endDiscussionLocked(fmt.Sprintf("%d周が終わったので、ディスカッションを終了しました。続ける場合はもう一度開始してください。", d.MaxRounds), "max_rounds")
	default:
		d.Round++
		d.idx, d.passes = 0, 0
		d.Next = d.Order[0]
		r.pushStatusLocked()
	}
}

func (r *Room) endDiscussionLocked(msg, reason string) {
	r.log.Info("discussion.end", "reason", reason, "round", r.disc.Round)
	r.disc = nil
	r.postLocked("system", msg, "system")
	r.pushStatusLocked()
}

func (r *Room) runTurn(ctx context.Context, a *Agent) turnOutcome {
	r.mu.Lock()
	if a.paused || a.interactive { // 順番が回ってきた時点で一時停止中・対話モード中なら飛ばす
		r.mu.Unlock()
		return outcomeSkipped
	}
	rotateJob, ok := r.waitRotateLocked(ctx, a)
	if !ok {
		r.mu.Unlock()
		return outcomeCanceled
	}
	upto := len(r.messages)
	var delta []Message
	for _, m := range r.messages[a.cursor:upto] {
		if m.From != a.ID && m.Kind != "pass" {
			delta = append(delta, m)
		}
	}
	if len(delta) == 0 { // セッションの切り替えは予約したまま、新着があるターンまで持ち越す
		a.cursor = upto
		r.mu.Unlock()
		return outcomeSkipped
	}
	minutesPath := ""
	if rotateJob != nil {
		delta = r.commitRotateLocked(a, rotateJob, upto)
		minutesPath = rotateJob.path
	}
	if r.disc == nil && r.free == nil {
		r.hops++
	}
	r.turnSeq++
	turnID := fmt.Sprintf("turn-%d", r.turnSeq)
	a.state, a.since = "thinking", time.Now()
	token := newJobToken()
	r.jobs[token] = &jobTicket{agentID: a.ID, gen: r.gen, modelVer: a.modelVer}
	prompt := r.buildPrompt(a, delta, token, minutesPath)
	sessionID := a.sessionID
	modelSel := a.modelSel
	permission := a.permission
	timeout := r.turnTimeoutForLocked(a)
	workdir := r.workdir
	readUpTo := r.messages[upto-1].ID // delta があるので upto > 0
	gen := r.gen
	r.pushStatusLocked()
	r.mu.Unlock()
	defer func() { // ジョブの終了でトークンを無効にする（世代は進めない）
		r.mu.Lock()
		delete(r.jobs, token)
		r.mu.Unlock()
	}()

	log := r.log.With("turn_id", turnID, "agent", a.ID)
	log.Info("agent.turn.start", "session_id", sessionID, "model_sel", modelSel, "new_messages", len(delta), "prompt_chars", len([]rune(prompt)), "timeout", timeout.String())
	start := time.Now()
	live := r.liveFor(a.ID)
	live.add("info", fmt.Sprintf("=== %s 開始（モデル: %s、新着 %d件、上限 %v） ===", turnID, firstNonEmpty(modelSel, "既定"), len(delta), timeout))
	// 別窓に渡す行はすべてここを通し、ジョブ用トークンを伏せる（tool_use の引数やエラー文に含まれ得る）
	sink := func(stream, line string) { live.add(stream, strings.ReplaceAll(line, token, redactedToken)) }
	ctx = withTurnTimeout(ctx, timeout)
	ctx = withPermission(ctx, permission)
	ctx = withLiveSink(ctx, sink)
	ctx = withProgress(ctx, func(s string) {
		r.mu.Lock()
		defer r.mu.Unlock()
		if gen == r.gen && a.state == "thinking" {
			a.activity = s
			r.pushStatusLocked()
		}
	})
	var throttle partialThrottle
	ctx = withPartialSink(ctx, func(text string) { // 書きかけの本文を画面にだけ配信する（案5）
		throttle.push(text, func(text string) {
			r.mu.Lock()
			defer r.mu.Unlock()
			if gen == r.gen && a.state == "thinking" {
				r.broadcastLocked(Event{Type: "partial", Partial: &Partial{Agent: a.ID, Text: strings.ReplaceAll(text, token, redactedToken)}})
			}
		})
	})
	before := takeFileSnapshot(workdir, r.logDir)        // ターンの間に変わったファイルを添えるため（案 12）
	beforeData := fileContents.remember(workdir, before) // 差分を作るため、ターンの前の中身を控える（案 9）
	ctx = withEditSink(ctx, func(p string) {
		r.mu.Lock()
		defer r.mu.Unlock()
		if gen == r.gen && a.state == "thinking" && a.addEditingLocked(editDisplayPath(workdir, p)) {
			log.Info("agent.editing", "file", editDisplayPath(workdir, p))
			r.pushStatusLocked()
		}
	})
	res, err := a.Adapter.Run(ctx, prompt, sessionID, modelSel, workdir)
	var fileVers string
	if err == nil {
		var derr error
		if fileVers, derr = fileVersionsMessage(a.Name, a.ID, workdir, r.logDir, before, beforeData); derr != nil {
			log.Warn("filediff.write", "err", derr)
		}
	}
	if err != nil {
		sink("info", fmt.Sprintf("=== %s 失敗（%.1f秒）: %v ===", turnID, time.Since(start).Seconds(), err))
	} else {
		live.add("info", fmt.Sprintf("=== %s 終了（%.1f秒） ===", turnID, time.Since(start).Seconds()))
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	r.broadcastLocked(Event{Type: "partial", Partial: &Partial{Agent: a.ID}}) // 成功・失敗・中止のどれでも途中表示を消す（案5）
	// 使用量は結果を採用するかどうかに関わらずジョブ単位で集計する（失敗・キャンセルは取得できなかった扱い）
	a.usage.add(res.Usage)
	r.saveUsageLocked()
	if res.Usage.Context > 0 { // 文脈の大きさが取れる CLI はそれで判定する（合計はツール呼び出しの回数で膨らむ）
		a.lastInput = res.Usage.Context
	} else if res.Usage.Known {
		a.lastInput = res.Usage.Input
	}
	go r.refreshQuota(a, false)                                            // ジョブ終了時に利用枠も更新する（直近に取得済みなら省略）
	a.state, a.since, a.activity, a.editing = "idle", time.Time{}, "", nil // ターンが終わったので担当を外す
	if r.free != nil {
		a.state = "listening"
	}
	latency := time.Since(start).Milliseconds()
	log = log.With("usage_known", res.Usage.Known, "input_tokens", res.Usage.Input,
		"cached_input_tokens", res.Usage.CachedInput, "output_tokens", res.Usage.Output)
	if gen != r.gen {
		log.Info("agent.turn.end", "discarded", "reset", "latency_ms", latency)
		r.pushStatusLocked()
		return outcomeCanceled
	}
	if err != nil {
		// 失敗時は cursor を進めない（次回、同じ新着をもう一度渡す）
		if minutesPath != "" { // 切り替え後の最初のターンが失敗・停止したら、次のターンで同じ議事録を渡し直す
			a.rotatePending = true
		}
		code := ErrAgentFailed
		var ae *AgentError
		if errors.As(err, &ae) {
			code = ae.Code
		}
		errText := strings.ReplaceAll(err.Error(), token, redactedToken) // CLI のエラー出力にトークンが含まれ得る
		log.Error("agent.turn.end", "error_code", code, "error", errText, "latency_ms", latency)
		r.pushStatusLocked()
		if code == ErrAgentCanceled {
			return outcomeCanceled
		}
		// 利用枠の上限は待たないと直らないので、再試行の目印は付けない（案 8.3）
		retry := a.ID
		if code == ErrAgentQuota {
			retry = ""
		}
		r.appendMessageLocked(Message{From: "system", Kind: "system", Text: fmt.Sprintf("%s [%s] %s", a.Name, code, errText), Retry: retry})
		if code == ErrAgentQuota && !a.paused {
			r.pauseLocked(a, "quota")
		}
		return outcomeFailed
	}
	if res.SessionID != "" {
		if a.sessionID == "" { // 新しいセッションが始まった
			a.sessionStart = delta[0].ID
		}
		a.sessionID = res.SessionID
	}
	if res.Model != "" {
		a.model = res.Model
	}
	a.cursor = upto
	r.saveSessionLocked()
	reply := strings.ReplaceAll(cleanReply(a, res.Text), token, redactedToken) // トークンを発言に残さない
	log.Info("agent.turn.end", "session_id", a.sessionID, "model", res.Model, "reply_chars", len([]rune(reply)), "latency_ms", latency)
	if reply == "" || passRe.MatchString(reply) {
		if r.free == nil { // フリートークでは様子見を表示しない（会話が流れにくくなるため）
			r.postModelLocked(a.ID, res.Model, "（パス）", "pass", readUpTo)
		}
		r.postFileVersionsLocked(fileVers)
		r.pushStatusLocked()
		return outcomePass
	}
	r.postModelLocked(a.ID, res.Model, reply, "chat", readUpTo)
	r.postFileVersionsLocked(fileVers)
	if r.free != nil {
		r.hops++
	}
	// ディスカッション・フリートーク中は発言者の決め方が別なのでメンションで割り込ませない
	if r.disc == nil && r.free == nil {
		ids, all := r.mentions(reply, a.ID)
		if all {
			ids = r.activeIDs(a.ID)
		}
		r.enqueueLocked(ids)
	}
	r.pushStatusLocked()
	return outcomeSpoke
}

// postFileVersionsLocked は、ターンの間に変わったファイルの一覧をシステムメッセージで添える（なければ何もしない）。
func (r *Room) postFileVersionsLocked(text string) {
	if text != "" {
		r.postLocked("system", text, "system")
	}
}

// formatDelta は新着1件をプロンプト用に書く。例: [#12 codex（#10 まで読了）]: 本文
func formatDelta(m Message) string {
	var notes []string
	if m.ReplyTo > 0 {
		notes = append(notes, fmt.Sprintf("#%d への返信", m.ReplyTo))
	}
	if m.ReadUpTo > 0 {
		notes = append(notes, fmt.Sprintf("#%d まで読了", m.ReadUpTo))
	}
	if len(notes) > 0 {
		return fmt.Sprintf("[#%d %s（%s）]: %s", m.ID, m.From, strings.Join(notes, "、"), m.Text)
	}
	return fmt.Sprintf("[#%d %s]: %s", m.ID, m.From, m.Text)
}

var passRe = regexp.MustCompile(`(?i)^\[?pass\]?\.?$`)

// minutesPath が空でなければ、セッションを切り替えた直後として議事録のパスを添える
func (r *Room) buildPrompt(a *Agent, delta []Message, token, minutesPath string) string {
	var b strings.Builder
	if a.sessionID == "" {
		var others []string
		for _, o := range r.agents {
			if o.ID != a.ID && o.active() {
				others = append(others, fmt.Sprintf("@%s（%s）", o.ID, o.Name))
			}
		}
		fmt.Fprintf(&b, "あなたは「%s」(@%s) として、人間と複数のAIエージェントのグループチャットに参加しています。\n", a.Name, a.ID)
		fmt.Fprintf(&b, "参加者: 人間(@human)、%s\n\n", strings.Join(others, "、"))
		b.WriteString("ルール:\n")
		b.WriteString("- 「[#番号 名前]: 本文」の形式で渡されるのは、あなたが前回発言してからの新着メッセージです。\n")
		b.WriteString("- あなたの返答はそのままチャットに投稿されます。名前の接頭辞は付けず、本文だけを書いてください。\n")
		b.WriteString("- 通常のチャットでは、他のエージェントに意見や作業を求めたいときに @ID で呼びかけると、呼ばれた相手が次に発言します。必要がなければ呼びかけないでください。他の参加者の発言に触れるだけなら @ を付けずに名前で書いてください。\n")
		b.WriteString("- 全員への呼びかけには全員が同時に回答します。他のエージェントの回答は、次に発言するときに新着として届きます。\n")
		b.WriteString("- ディスカッション中は発言順が決まっています（[system] のメッセージで通知されます）。\n")
		b.WriteString("- フリートーク中は、全員が新着を見て、話したいときに自由に発言します。\n")
		b.WriteString("- 簡潔に、会話で使われている言語で答えてください。\n")
		b.WriteString("- 付け加えることが特にない場合は [pass] とだけ返してください。\n")
		fmt.Fprintf(&b, "- 作業ディレクトリ: %s\n\n", r.workdir)
	}
	if minutesPath != "" {
		b.WriteString("トークン消費を抑えるため、あなたの CLI セッションを切り替えました。これまでの会話は議事録にまとめてあります。\n")
		fmt.Fprintf(&b, "- 議事録: %s\n", minutesPath)
		b.WriteString("- 続きの作業に必要なら、まず議事録を読んでください。以下の新着には、直近の発言（あなた自身の発言を含む）を載せています。\n\n")
	}
	b.WriteString("--- 新着メッセージ（#番号は発言ID。「#N まで読了」は、その発言が #N までを読んで書かれたことを示します。それより後の発言は反映されていません）---\n")
	for i, m := range delta {
		if i > 0 {
			b.WriteString("\n\n")
		}
		b.WriteString(formatDelta(m))
	}
	b.WriteString("\n--- ここまで ---\n\n")
	r.writeSelfStatus(&b, a, token)
	r.writeLeader(&b, a)
	r.writeEditingLocked(&b, a)
	if d := r.disc; d != nil {
		fmt.Fprintf(&b, "ディスカッション中です（第%d周／最大%d周）。お題: %s\n", d.Round, d.MaxRounds, d.Topic)
		b.WriteString("他の参加者の意見に具体的に応じて、同意・反論・新しい論点・具体案のいずれかを述べてください。")
		b.WriteString("既に出た内容の繰り返しは避け、議論が出尽くしたと思えば [pass] と返してください。\n")
	}
	if r.free != nil {
		b.WriteString("フリートーク中です。雑談のように、話したいことがあれば自由に発言してください。")
		b.WriteString("1〜3文程度で短く、会話のテンポを大切にしてください。質問・同意・反論・話題の展開どれでも構いません。")
		b.WriteString("他の人が既に言ったことの繰り返しになる場合や、今は聞き役でよい場合は [pass] とだけ返してください（パスは表示されません）。\n")
	}
	fmt.Fprintf(&b, "@%s として発言してください。", a.ID)
	return b.String()
}

// writeSelfStatus はエージェントが自分で判断できるよう、利用状況とモデルの候補を添える
func (r *Room) writeSelfStatus(b *strings.Builder, a *Agent, token string) {
	b.WriteString("あなたの状況（AI Agent Room が記録）:\n")
	fmt.Fprintf(b, "- 累積トークン消費: %s\n", a.usage.summary())
	fmt.Fprintf(b, "- プランの残り利用枠: %s\n", a.quota.summary())
	cur := firstNonEmpty(a.modelSel, "CLI の既定")
	if a.model != "" {
		cur += "（直近の実際のモデル: " + a.model + "）"
	}
	fmt.Fprintf(b, "- 使用中のモデル: %s\n", cur)
	if ms := a.Adapter.Models(); len(ms) > 0 {
		ids := make([]string, len(ms))
		for i, m := range ms {
			ids[i] = m.ID
		}
		fmt.Fprintf(b, "- モデルの候補（取得元: %s。アカウントで利用できるとは限りません）: %s\n",
			a.Adapter.ModelSource(), strings.Join(ids, ", "))
		if selfToolURL != "" && a.Adapter.SelfTool() {
			fmt.Fprintf(b, "- モデルの切替: 作業内容と利用状況を見て、必要なら AI Agent Room の %s ツールを token=%q で呼ぶと、次の発言から切り替わります（このトークンは今回の発言中だけ有効です。発言には書かないでください）。\n",
				selfToolName, token)
		}
	}
	if selfToolURL != "" && a.Adapter.SelfTool() {
		r.writeLeases(b, token)
	}
	b.WriteString("\n")
}

// writeLeader は進行役の指定と役割を添える。設定は会話の途中でも変わるため、毎ターン書く
func (r *Room) writeLeader(b *strings.Builder, a *Agent) {
	l := r.agent(r.leader)
	if l == nil {
		return
	}
	if l.ID == a.ID {
		b.WriteString("進行役: あなたです。人間の依頼を作業に分けて担当を @ID で割り振り、未完了の作業がある間は次の担当と作業・完了条件を示してください。")
		b.WriteString("人間の依頼の範囲内なら、進めるかどうかはあなたが判断して構いません。目的や範囲を変える判断が必要なときだけ人間に確認してください。")
		b.WriteString("すべて完了したら、完了したことを伝えて止まってください。\n\n")
		return
	}
	fmt.Fprintf(b, "進行役: @%s（%s）。作業の割り振りと、進めるかどうかの判断は進行役に従ってください。割り振られた作業は確認を待たずに進め、結果を報告してください。\n\n", l.ID, l.Name)
}

// cleanReply はエージェントが付けてしまった「[claude]:」のような接頭辞を取り除く
func cleanReply(a *Agent, text string) string {
	t := strings.TrimSpace(text)
	names := append([]string{regexp.QuoteMeta(a.Name)}, a.Aliases...)
	re := regexp.MustCompile(`(?i)^\[?(` + strings.Join(names, "|") + `)\]?\s*[:：]\s*`)
	return strings.TrimSpace(re.ReplaceAllString(t, ""))
}

// recentMessages は msgs のうち、パスを除いた直近 n 件を返す
func recentMessages(msgs []Message, n int) []Message {
	var res []Message
	for i := len(msgs) - 1; i >= 0 && len(res) < n; i-- {
		if msgs[i].Kind != "pass" {
			res = append(res, msgs[i])
		}
	}
	slices.Reverse(res)
	return res
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}
