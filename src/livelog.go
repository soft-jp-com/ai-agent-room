package main

// エージェントの CLI 出力の別窓表示。ターン中の stdout / stderr を1行ずつ、エージェントごとに直近 liveMaxLines 行だけ
// メモリに保持し、別窓（web/live.html）が開いている間だけ GET /api/agents/{id}/live（SSE）で流す。
// ファイルには保存しない（量が多いため）。

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"
)

const (
	liveMaxLines     = 1000 // エージェントごとに保持する行数
	liveMaxLineChars = 2000 // 1行の表示上限（超えた分は省略）
	liveMaxStrChars  = 300  // JSON の行を縮めるときの、文字列1つの上限
)

// liveLine は出力1行
type liveLine struct {
	Seq    int    `json:"seq"`
	Time   string `json:"time"`   // 受信時刻（HH:MM:SS）
	Stream string `json:"stream"` // stdout / stderr / info（ターンの開始・終了）
	Text   string `json:"text"`
}

// liveLog はエージェント1つ分の出力の保持と配信
type liveLog struct {
	mu    sync.Mutex
	lines []liveLine
	seq   int
	subs  map[chan liveLine]struct{}
}

func (l *liveLog) add(stream, text string) {
	text = shortenLiveLine(text)
	l.mu.Lock()
	defer l.mu.Unlock()
	l.seq++
	ln := liveLine{Seq: l.seq, Time: time.Now().Format("15:04:05"), Stream: stream, Text: text}
	l.lines = append(l.lines, ln)
	if len(l.lines) > liveMaxLines {
		l.lines = append([]liveLine(nil), l.lines[len(l.lines)-liveMaxLines:]...)
	}
	for ch := range l.subs {
		select {
		case ch <- ln:
		default: // 詰まった窓は取りこぼす（開き直せば直近の行を受け取れる）
		}
	}
}

// shortenLiveLine は長すぎる行を縮める。JSON の行は中の長い文字列だけを縮めて、JSON として読める形を保つ。
// それでも長い行と JSON でない行は、先頭 liveMaxLineChars 文字で切る
func shortenLiveLine(text string) string {
	if len([]rune(text)) <= liveMaxLineChars {
		return text
	}
	if t := bytes.TrimSpace([]byte(text)); len(t) > 0 && (t[0] == '{' || t[0] == '[') && json.Valid(t) {
		if b, err := shrinkJSON(t); err == nil && len([]rune(string(b))) <= liveMaxLineChars {
			return string(b)
		}
	}
	r := []rune(text)
	return string(r[:liveMaxLineChars]) + fmt.Sprintf("…（%d文字省略）", len(r)-liveMaxLineChars)
}

// shrinkJSON は JSON の値の中の、liveMaxStrChars 文字を超える文字列を縮める。キーの順番は変えない
func shrinkJSON(raw []byte) ([]byte, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return raw, nil
	}
	switch raw[0] {
	case '"':
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return nil, err
		}
		if r := []rune(s); len(r) > liveMaxStrChars {
			return json.Marshal(string(r[:liveMaxStrChars]) + fmt.Sprintf("…（%d文字省略）", len(r)-liveMaxStrChars))
		}
		return raw, nil
	case '{', '[':
		dec := json.NewDecoder(bytes.NewReader(raw))
		if _, err := dec.Token(); err != nil {
			return nil, err
		}
		var out bytes.Buffer
		out.WriteByte(raw[0])
		for i := 0; dec.More(); i++ {
			if i > 0 {
				out.WriteByte(',')
			}
			if raw[0] == '{' {
				key, err := dec.Token()
				if err != nil {
					return nil, err
				}
				kb, _ := json.Marshal(key)
				out.Write(kb)
				out.WriteByte(':')
			}
			var v json.RawMessage
			if err := dec.Decode(&v); err != nil {
				return nil, err
			}
			vb, err := shrinkJSON(v)
			if err != nil {
				return nil, err
			}
			out.Write(vb)
		}
		if raw[0] == '{' {
			out.WriteByte('}')
		} else {
			out.WriteByte(']')
		}
		return out.Bytes(), nil
	}
	return raw, nil
}

// subscribe は保持している行と、以降の行を受け取るチャネルを返す
func (l *liveLog) subscribe() ([]liveLine, chan liveLine, func()) {
	l.mu.Lock()
	defer l.mu.Unlock()
	ch := make(chan liveLine, 512)
	if l.subs == nil {
		l.subs = map[chan liveLine]struct{}{}
	}
	l.subs[ch] = struct{}{}
	backlog := append([]liveLine(nil), l.lines...)
	return backlog, ch, func() {
		l.mu.Lock()
		delete(l.subs, ch)
		l.mu.Unlock()
	}
}

// liveFor はエージェントの出力の保持先を返す（なければ作る）
func (r *Room) liveFor(id string) *liveLog {
	r.liveMu.Lock()
	defer r.liveMu.Unlock()
	if r.live == nil {
		r.live = map[string]*liveLog{}
	}
	l := r.live[id]
	if l == nil {
		l = &liveLog{}
		r.live[id] = l
	}
	return l
}

// handleLive はエージェントの CLI 出力を SSE で流す。最初に保持している直近の行を送る
func (room *Room) handleLive(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	room.mu.Lock()
	known := room.agent(id) != nil
	room.mu.Unlock()
	if !known {
		writeError(w, r, http.StatusNotFound, "AGENT_NOT_FOUND", ErrAgentNotFound.Error())
		return
	}
	fl, ok := w.(http.Flusher)
	if !ok {
		writeError(w, r, http.StatusInternalServerError, "STREAM_UNSUPPORTED", "ストリーミングに対応していません")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	backlog, ch, unsub := room.liveFor(id).subscribe()
	defer unsub()
	send := func(ln liveLine) {
		b, _ := json.Marshal(ln)
		fmt.Fprintf(w, "data: %s\n\n", b)
	}
	for _, ln := range backlog {
		send(ln)
	}
	fl.Flush()
	ping := time.NewTicker(25 * time.Second)
	defer ping.Stop()
	for {
		select {
		case ln := <-ch:
			send(ln)
			fl.Flush()
		case <-ping.C:
			io.WriteString(w, ": ping\n\n")
			fl.Flush()
		case <-r.Context().Done():
			return
		}
	}
}
