package main

// logs/ 内の状態ファイル（usage.json・session.json）の保存。
// 一時ファイルに書いてから置き換えるが、Dropbox の同期などでファイルが一時的に
// 開かれていると Windows では置き換えが Access is denied で失敗するため、
// 権限エラーのときだけ短い間隔で数回やり直す。

import (
	"errors"
	"os"
	"time"
)

const (
	stateRenameAttempts = 5
	stateRenameInterval = 50 * time.Millisecond
)

// writeStateFile は data を path に置き換え保存する。失敗したら一時ファイルを消し、
// 最後のエラー（一時ファイルを消せなかった場合はそのエラーも）を返す
func writeStateFile(path string, data []byte) error {
	tmp := path + ".tmp"
	err := os.WriteFile(tmp, data, 0o644)
	if err == nil {
		for i := range stateRenameAttempts {
			if i > 0 {
				time.Sleep(stateRenameInterval)
			}
			if err = os.Rename(tmp, path); err == nil || !errors.Is(err, os.ErrPermission) {
				break
			}
		}
	}
	if err != nil {
		if rerr := os.Remove(tmp); rerr != nil && !errors.Is(rerr, os.ErrNotExist) {
			err = errors.Join(err, rerr)
		}
	}
	return err
}
