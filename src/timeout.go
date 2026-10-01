package main

// 1ターンの上限時間と、失敗したターンの再試行（案 8.3）。
//
// 以前は 15 分の固定値だったため、CLI が固まったときも 15 分待つことになり、CLI ごとに変えることもできなかった。
//   - 会話全体の既定値（defaultTurnTimeout）を設定で変えられる
//   - エージェントごとに上書きできる（遅い CLI だけ長くする、など）
//   - 失敗したターンは、画面の［再試行］でもう一度起動できる（失敗時は cursor を進めていないので同じ新着を渡し直せる）
//
// 設定の保存先は settings.json（設定フォルダ。エージェントが書き換えられない場所。settings.go）。

import (
	"errors"
	"fmt"
	"time"
)

var (
	ErrAgentThinking = errors.New("このエージェントは実行中です")
	ErrAgentPaused   = errors.New("一時停止中のエージェントは起動できません")
	ErrNotRetryable  = errors.New("再試行できる状態ではありません")
)

// turnTimeoutForLocked は a の1ターンの上限時間を返す（エージェントごとの設定がなければ会話全体の設定）
func (r *Room) turnTimeoutForLocked(a *Agent) time.Duration {
	if a.timeoutSec > 0 {
		return clampTurnTimeout(a.timeoutSec)
	}
	if r.turnTimeout > 0 {
		return r.turnTimeout
	}
	return defaultTurnTimeout
}

// SetTurnTimeout は会話全体の1ターンの上限時間（秒）を設定する。範囲外は丸める
func (r *Room) SetTurnTimeout(sec int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.turnTimeout = clampTurnTimeout(sec)
	r.log.Info("settings.turn_timeout", "timeout", r.turnTimeout.String())
	r.pushStatusLocked()
}

// SetAgentTimeout はエージェントごとの上限時間（秒）を設定する。0 で会話全体の設定に戻す
func (r *Room) SetAgentTimeout(id string, sec int) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	a := r.agent(id)
	if a == nil {
		return ErrAgentNotFound
	}
	if sec <= 0 {
		a.timeoutSec = 0
	} else {
		a.timeoutSec = int(clampTurnTimeout(sec) / time.Second)
	}
	r.log.Info("agent.turn_timeout", "agent", a.ID, "timeout_sec", a.timeoutSec)
	r.pushStatusLocked()
	return nil
}

// RetryAgent は失敗したターンをもう一度実行させる。
// 失敗時は cursor を進めていないので、同じ新着がそのまま渡り直る。
// フリートークで外れたエージェント（連続失敗で待機ループを抜けたもの）は、待機ループを始め直す。
func (r *Room) RetryAgent(id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	a := r.agent(id)
	if a == nil {
		return ErrAgentNotFound
	}
	if a.state == "unavailable" {
		return ErrAgentNotFound
	}
	if a.paused {
		return ErrAgentPaused
	}
	if a.state == "thinking" {
		return ErrAgentThinking
	}
	r.log.Info("agent.retry", "agent", a.ID)
	r.postLocked("system", fmt.Sprintf("%s をもう一度起動します。", a.Name), "system")
	if r.free != nil { // フリートーク中は待機ループが起動する（外れていたら始め直す）
		if a.state == "idle" {
			a.state = "listening"
			go r.freeLoop(r.free, a)
		}
		r.cond.Broadcast()
		r.pushStatusLocked()
		return nil
	}
	if r.disc != nil { // ディスカッションは発言順で回るので、順番が来たときに渡し直される
		r.pushStatusLocked()
		return nil
	}
	r.hops = 0 // 人間の操作による再試行なので、自動ターンの上限で止めない
	r.enqueueLocked([]string{a.ID})
	return nil
}
