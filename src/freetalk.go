package main

// フリートーク: 全エージェントが常に待機し、新しい発言を見て話したいときに自由に発言する。
//
// CLI エージェントは話しかけられない限り発言できないため、エージェントごとに待機ループを動かし、
// 他の参加者の新しい発言があれば「話したいことがあれば発言、なければ [pass]」と尋ねる。
//   - 会話が delay だけ途切れてから考え始める（人間が割り込む余地を残す）
//   - パスは表示しない。全員がパスすると会話は自然に止まり、次の発言を待つ
//   - 人間の発言1回あたりのエージェントの発言数は maxHops まで（暴走防止）

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"
)

// FreeTalk はフリートークの状態
type FreeTalk struct {
	Topic     string `json:"topic"`
	StartedAt int64  `json:"started_at"` // UnixMilli

	limitNotified bool // 発言数の上限に達したことを通知済みか
	// firstIDs は、直近の人間の発言に先に答えるエージェント（進行役、または名指しされた人）。
	// 全員がそのあとのターンを終えるまで、ほかのエージェントは発言を考え始めない（ルール1。回答の重複を防ぐ）
	firstIDs   []string
	firstSince time.Time // firstIDs を決めた時刻（これより前に始まったターンは、答えたことにしない）
	ctx        context.Context
	cancel     context.CancelFunc
}

const freeTalkMaxFailures = 3 // 連続で失敗したエージェントはフリートークから外す

// StartFreeTalk はフリートークを開始する。topic が空でなければ人間の発言として投稿する。
func (r *Room) StartFreeTalk(topic string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.disc != nil || r.free != nil {
		return ErrDiscussionBusy
	}
	ids := r.activeIDs("")
	if len(ids) == 0 {
		return ErrNoAgents
	}
	r.queue = nil // チャットの発言待ちはフリートークに置き換える
	ctx, cancel := context.WithCancel(context.Background())
	ft := &FreeTalk{Topic: topic, StartedAt: time.Now().UnixMilli(), ctx: ctx, cancel: cancel}
	r.free = ft
	r.hops = 0
	if topic != "" {
		r.postLocked("human", topic, "chat")
		r.checkRotateLocked()
	}
	r.postLocked("system", fmt.Sprintf("フリートーク開始: 全員が新しい発言を見て、話したいときに自由に発言します。"+
		"エージェントの発言は人間の発言1回あたり%d回まで（上限の設定で変更可）。「停止」で終了します。", r.maxHops), "system")
	r.log.Info("free_talk.start", "agents", len(ids))
	for _, id := range ids {
		a := r.agent(id)
		if a.state == "idle" {
			a.state = "listening"
		}
		go r.freeLoop(ft, a)
	}
	r.pushStatusLocked()
	return nil
}

func (r *Room) endFreeTalkLocked(reason string) {
	ft := r.free
	if ft == nil {
		return
	}
	ft.cancel()
	r.free = nil
	for _, a := range r.agents {
		if a.state == "listening" || a.state == "waiting" {
			a.state = "idle"
		}
		a.waitUntil = time.Time{}
	}
	r.log.Info("free_talk.end", "reason", reason)
	r.cond.Broadcast() // 待機中のループを終了させる
}

// hasNewChatLocked は a がまだ受け取っていない、他の参加者の発言があるかを返す。
// システムメッセージだけでは発言のきっかけにしない。コマンドの実行結果は、呼び出す相手（進行役）だけのきっかけにする
func (r *Room) hasNewChatLocked(a *Agent) bool {
	for _, m := range r.messages[a.cursor:] {
		if m.From != a.ID && m.Kind == "chat" {
			return true
		}
		if m.Kind == "command_result" && r.commandNotifyTargetLocked(m) == a.ID {
			return true
		}
	}
	return false
}

// firstAnswerTimeout を過ぎたら、先に答える人を待たずにほかのエージェントも発言できる（応答しない場合の保険）
const firstAnswerTimeout = 15 * time.Minute

// setFirstRespondersLocked は人間の発言 text に先に答えるエージェントを決める。
// 名指し（@ID）があればその人、なければ進行役。@all・宛先も進行役もなければ、全員が同時に考える
func (r *Room) setFirstRespondersLocked(text string) {
	ft := r.free
	if ft == nil {
		return
	}
	ids, all := r.mentions(text, "human")
	if all {
		ids = nil
	} else if len(ids) == 0 && r.leader != "" {
		ids = []string{r.leader}
	}
	ft.firstIDs, ft.firstSince = ids, time.Now()
	if len(ids) > 0 {
		r.log.Info("free_talk.first_responders", "agents", strings.Join(ids, ","))
	}
}

// firstAnsweredLocked は、先に答えるエージェント a のターン（started に開始）が終わったことを記録する
func (r *Room) firstAnsweredLocked(ft *FreeTalk, a *Agent, started time.Time) {
	if started.Before(ft.firstSince) {
		return // 人間の発言より前に始まったターンは、その発言に答えていない
	}
	if i := slices.Index(ft.firstIDs, a.ID); i >= 0 {
		ft.firstIDs = slices.Delete(ft.firstIDs, i, i+1)
		r.cond.Broadcast() // 待っているほかのエージェントを起こす
	}
}

// waitsForFirstLocked は、a が先に答える人の回答を待つべきかを返す。
// 先に答える人が一時停止・参加できない場合や、時間切れのときは待たない
func (r *Room) waitsForFirstLocked(ft *FreeTalk, a *Agent) bool {
	if len(ft.firstIDs) == 0 || slices.Contains(ft.firstIDs, a.ID) {
		return false
	}
	if time.Since(ft.firstSince) > firstAnswerTimeout {
		ft.firstIDs = nil
		return false
	}
	for _, id := range ft.firstIDs {
		if f := r.agent(id); f != nil && f.active() {
			return true
		}
	}
	return false
}

// freeLoop はフリートーク中、エージェント a の発言のきっかけを待ち続ける。
func (r *Room) freeLoop(ft *FreeTalk, a *Agent) {
	r.mu.Lock()
	defer r.mu.Unlock()
	failures := 0
	for r.free == ft {
		if a.paused || a.interactive { // 一時停止・対話モードになったら抜ける（戻ると SetPaused / waitInteractive が聞き直させる）
			if a.state == "listening" || a.state == "waiting" {
				a.state, a.waitUntil = "idle", time.Time{}
			}
			r.pushStatusLocked()
			return
		}
		// 新着がない / 考え中（チャットのターンが残っている場合など）なら次の発言まで待つ
		if a.state == "thinking" || !r.hasNewChatLocked(a) {
			r.cond.Wait()
			continue
		}
		// 人間の発言には、進行役（または名指しされた人）が先に答える。その回答が出るまで待つ
		if r.waitsForFirstLocked(ft, a) {
			r.condWaitTimeout(firstAnswerTimeout)
			continue
		}
		if r.hops >= r.maxHops {
			if !ft.limitNotified {
				ft.limitNotified = true
				r.postLocked("system", fmt.Sprintf("エージェントの発言が上限（%d回）に達しました。人間が発言すると再開します。", r.maxHops), "system")
				r.pushStatusLocked()
			}
			r.cond.Wait()
			continue
		}
		// 会話が delay だけ途切れるまで待つ。待っている間に新しい発言があれば待ち直す
		if wait := time.Until(r.lastPost.Add(r.delay)); wait > 0 {
			a.state, a.waitUntil = "waiting", time.Now().Add(wait)
			r.pushStatusLocked()
			r.mu.Unlock()
			select {
			case <-time.After(wait):
			case <-ft.ctx.Done():
			}
			r.mu.Lock()
			if r.free == ft {
				a.state, a.waitUntil = "listening", time.Time{}
			}
			continue
		}

		started := time.Now()
		r.mu.Unlock()
		outcome := r.runTurn(ft.ctx, a)
		r.mu.Lock()
		r.firstAnsweredLocked(ft, a, started)

		if outcome != outcomeFailed {
			failures = 0
			continue
		}
		failures++
		if failures >= freeTalkMaxFailures && r.free == ft {
			a.state = "idle"
			r.postLocked("system", fmt.Sprintf("%s が%d回続けて失敗したので、フリートークから外しました。", a.Name, failures), "system")
			r.log.Error("free_talk.agent_removed", "agent", a.ID, "failures", failures)
			if i := slices.Index(ft.firstIDs, a.ID); i >= 0 { // 外したエージェントの回答は待たない
				ft.firstIDs = slices.Delete(ft.firstIDs, i, i+1)
				r.cond.Broadcast()
			}
			r.pushStatusLocked()
			return
		}
	}
}

// condWaitTimeout は r.cond.Wait を、d が過ぎたら起きるようにして呼ぶ（r.mu を持った状態で呼ぶ）
func (r *Room) condWaitTimeout(d time.Duration) {
	t := time.AfterFunc(d, func() {
		r.mu.Lock()
		r.cond.Broadcast()
		r.mu.Unlock()
	})
	r.cond.Wait()
	t.Stop()
}
