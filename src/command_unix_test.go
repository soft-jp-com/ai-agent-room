//go:build !windows

package main

import (
	"errors"
	"testing"
)

// cmd・bat のブロックは Windows 以外では実行しない（画面も［実行］を押せなくする）
func TestRunBlockCmdUnsupportedOS(t *testing.T) {
	r, _ := newTestRoom(t)
	m := postChat(r, "human", "```cmd\necho 1\n```\n```bat\necho 2\n```")
	for i := range 2 {
		if _, err := r.RunBlock(m.ID, i, CommandRequest{}); !errors.Is(err, ErrCommandUnsupportedOS) {
			t.Fatalf("ブロック%d: %v", i+1, err)
		}
	}
}
