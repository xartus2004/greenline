// Package forge holds code-host adapters. Each implements queue.Forge plus
// whatever the runner needs to materialise a candidate commit.
package forge

import "github.com/xartus2004/greenline/queue"

// Merger builds the commit for a group of PR items on top of a parent. It
// must be deterministic: the same parent and items always yield the same
// commit, which is what lets speculative runs and later merges agree.
type Merger interface {
	Apply(parent string, group []queue.Item) (commit string, err error)
}

// ConflictError is returned by Merger.Apply when an item does not merge cleanly.
type ConflictError struct{ PR queue.PRID }

func (e *ConflictError) Error() string { return "merge conflict applying PR" }
