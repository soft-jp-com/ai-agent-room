package main

// ファイルの版の記録: エージェントのターンの前後で作業ディレクトリのファイルを記録し、
// ターンの間に変わったファイルと、その更新時刻・ハッシュの先頭をシステムメッセージで添える。
// レビュー役が、自分の読んだファイルが修正の前か後かを見分けられるようにする。
//
// 前後の比較は更新時刻と大きさだけで行い、ハッシュは変わったファイルだけ計算する。
// 同時に動いている別のエージェントや人間の変更も、この間に起きれば同じく表示される。

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	fileSnapMaxFiles  = 20000 // これを超えるディレクトリでは記録をやめる（走査に時間がかかるため）
	fileVerMaxList    = 20    // システムメッセージに載せるファイルの上限
	fileVerHashLen    = 8     // 表示するハッシュの桁数
	fileVerMaxHashLen = 64 << 20
)

// 走査しないディレクトリ（名前が一致したもの）。点で始まるディレクトリも飛ばす。
var fileSnapSkipDirs = map[string]bool{"node_modules": true, "vendor": true}

type fileStamp struct {
	mod  time.Time
	size int64
}

// fileSnapshot は作業ディレクトリ内のファイルの更新時刻と大きさ。キーは workdir からの相対パス（/ 区切り）。
type fileSnapshot map[string]fileStamp

// takeFileSnapshot は workdir 以下を走査して記録する。skip は走査しない絶対パス（ログの置き場など）。
// ファイル数が上限を超えたら nil を返す（記録しない）。
func takeFileSnapshot(workdir string, skip ...string) fileSnapshot {
	if workdir == "" {
		return nil
	}
	skipAbs := map[string]bool{}
	for _, s := range skip {
		if s != "" {
			if abs, err := filepath.Abs(s); err == nil {
				skipAbs[strings.ToLower(abs)] = true
			}
		}
	}
	snap := fileSnapshot{}
	tooMany := false
	filepath.WalkDir(workdir, func(path string, d fs.DirEntry, err error) error {
		if err != nil { // 読めないディレクトリ・ファイルは飛ばす
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			if path != workdir && (strings.HasPrefix(d.Name(), ".") || fileSnapSkipDirs[d.Name()] || skipAbs[strings.ToLower(path)]) {
				return fs.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		if len(snap) >= fileSnapMaxFiles {
			tooMany = true
			return fs.SkipAll
		}
		rel, err := filepath.Rel(workdir, path)
		if err != nil {
			return nil
		}
		snap[filepath.ToSlash(rel)] = fileStamp{mod: info.ModTime(), size: info.Size()}
		return nil
	})
	if tooMany {
		return nil
	}
	return snap
}

// fileChange は前後の記録の差分1件。
type fileChange struct {
	path    string // workdir からの相対パス
	deleted bool
	added   bool
	mod     time.Time
	hash    string // 先頭 fileVerHashLen 桁（計算できなければ空）
	// 差分（案 9）。前後の中身がそろったときだけ hasDiff が true になる
	hasDiff bool
	plus    int // 追加した行数
	minus   int // 削除した行数
}

// diffFileSnapshots は before と after の差分を、パスの順に返す。ハッシュは workdir のファイルから計算する。
func diffFileSnapshots(workdir string, before, after fileSnapshot) []fileChange {
	if before == nil || after == nil {
		return nil
	}
	var out []fileChange
	for p, st := range after {
		old, ok := before[p]
		if ok && old.mod.Equal(st.mod) && old.size == st.size {
			continue
		}
		out = append(out, fileChange{path: p, added: !ok, mod: st.mod, hash: shortFileHash(filepath.Join(workdir, filepath.FromSlash(p)), st.size)})
	}
	for p := range before {
		if _, ok := after[p]; !ok {
			out = append(out, fileChange{path: p, deleted: true})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].path < out[j].path })
	return out
}

func shortFileHash(path string, size int64) string {
	if size > fileVerMaxHashLen {
		return ""
	}
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return ""
	}
	return hex.EncodeToString(h.Sum(nil))[:fileVerHashLen]
}

// formatFileVersions は差分をシステムメッセージの本文にする。変更がなければ空を返す。
func formatFileVersions(name string, changes []fileChange) string {
	if len(changes) == 0 {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s のターンの間に変わったファイル（%d件）:", name, len(changes))
	for i, c := range changes {
		if i >= fileVerMaxList {
			fmt.Fprintf(&b, "\n- ほか %d件", len(changes)-fileVerMaxList)
			break
		}
		switch {
		case c.deleted:
			fmt.Fprintf(&b, "\n- `%s` 削除%s", c.path, c.lineCounts())
		default:
			label := "更新"
			if c.added {
				label = "新規"
			}
			hash := c.hash
			if hash == "" {
				hash = "-"
			}
			fmt.Fprintf(&b, "\n- `%s` %s %s sha256:%s%s", c.path, label, c.mod.Format("15:04:05"), hash, c.lineCounts())
		}
	}
	return b.String()
}

// lineCounts は差分の行数の表示（差分がなければ空）
func (c fileChange) lineCounts() string {
	if !c.hasDiff {
		return ""
	}
	return fmt.Sprintf(" +%d -%d", c.plus, c.minus)
}
