package flist

import (
	"sort"
)

// IncrementalFileList tracks parent-directory dependencies for incremental
// recursion (protocol >= 30, INC_RECURSE). Entries are pushed as their file
// list segments arrive on the wire and are only yielded once their parent
// directory has been yielded, so a consumer can rely on directories arriving
// before their contents even though segments may arrive out of order.
//
// It is a port of oc-rsync
// crates/protocol/src/flist/incremental/mod.rs (IncrementalFileList).
//
// The zero value is NOT usable; construct with NewIncrementalFileList.
// The implementation is not safe for concurrent use; the receiver wraps it
// with a sync.Mutex/sync.Cond pair.
type IncrementalFileList struct {
	ready    []*FileEntry
	readyIdx int // queue head into ready, so pops don't shift the slice

	pending     map[string][]*FileEntry
	createdDirs map[string]bool

	yielded       int
	pendingCount  int
	finalizedPlaceholders int
}

// NewIncrementalFileList returns a ready-to-use incremental list. The root
// directory "." (and the empty name) are implicitly available, mirroring
// oc-rsync's constructor.
func NewIncrementalFileList() *IncrementalFileList {
	return &IncrementalFileList{
		pending:     map[string][]*FileEntry{},
		createdDirs: map[string]bool{"": true, ".": true},
	}
}

// Push adds an entry. If its parent directory is already available the entry
// goes to the ready queue (and a directory immediately releases its own
// pending children); otherwise it is held in the pending map. Push returns
// whether the entry became ready immediately.
func (l *IncrementalFileList) Push(f *FileEntry) bool {
	parent := ParentPath(f.Name)
	if !l.createdDirs[parent] {
		l.pending[parent] = append(l.pending[parent], f)
		l.pendingCount++
		return false
	}
	l.pushReady(f)
	return true
}

func (l *IncrementalFileList) pushReady(f *FileEntry) {
	l.ready = append(l.ready, f)
	if f.isDir() {
		l.createdDirs[f.Name] = true
		l.releasePending(f.Name)
	}
}

// releasePending moves every entry waiting on dir to the ready queue,
// recursively for directory children (oc-rsync release_pending_children).
func (l *IncrementalFileList) releasePending(dir string) {
	children, ok := l.pending[dir]
	if !ok {
		return
	}
	delete(l.pending, dir)
	l.pendingCount -= len(children)
	for _, child := range children {
		l.pushReady(child)
	}
}

// MarkDirCreated records a directory as available without going through Push
// (used for destination directories that already exist) and releases its
// pending children.
func (l *IncrementalFileList) MarkDirCreated(dir string) {
	l.createdDirs[dir] = true
	l.releasePending(dir)
}

// Pop returns the next ready entry, or nil when the queue is empty.
func (l *IncrementalFileList) Pop() *FileEntry {
	if l.readyIdx >= len(l.ready) {
		return nil
	}
	f := l.ready[l.readyIdx]
	l.ready[l.readyIdx] = nil
	l.readyIdx++
	l.yielded++
	if l.readyIdx == len(l.ready) {
		l.ready = l.ready[:0]
		l.readyIdx = 0
	}
	return f
}

// ReadyLen reports how many entries are ready for consumption.
func (l *IncrementalFileList) ReadyLen() int { return len(l.ready) - l.readyIdx }

// PendingCount reports how many entries still wait for a parent directory.
func (l *IncrementalFileList) PendingCount() int { return l.pendingCount }

// Finalize resolves entries whose parent directories never arrived: every
// missing ancestor is synthesized as a mode-0755 placeholder directory
// (oc-rsync finalize), releasing the orphans into the ready queue. Call it
// once all segments have been consumed and the ready queue is drained.
func (l *IncrementalFileList) Finalize() {
	if len(l.pending) == 0 {
		return
	}
	parents := make([]string, 0, len(l.pending))
	for p := range l.pending {
		parents = append(parents, p)
	}
	// Shallowest first so ancestors are created top-down.
	sort.Slice(parents, func(i, j int) bool {
		return slashCount(parents[i]) < slashCount(parents[j])
	})
	for _, p := range parents {
		if l.createdDirs[p] {
			continue
		}
		// Synthesize all missing ancestors from the root downward.
		for _, anc := range missingAncestors(p, l.createdDirs) {
			l.createdDirs[anc] = true
			l.finalizedPlaceholders++
			l.releasePending(anc)
		}
		l.createdDirs[p] = true
		l.finalizedPlaceholders++
		l.releasePending(p)
	}
}

// slashCount is a tiny helper for the finalize sort.
func slashCount(s string) int {
	n := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '/' {
			n++
		}
	}
	return n
}

// missingAncestors returns the ancestor directories of path that are not in
// created, top-down (oc-rsync collect_missing_ancestors). path itself is not
// included.
func missingAncestors(path string, created map[string]bool) []string {
	var out []string
	for i := len(path) - 1; i > 0; i-- {
		if path[i] != '/' {
			continue
		}
		parent := path[:i]
		if !created[parent] {
			out = append(out, parent)
		}
	}
	// The loop discovers deepest-first; reverse to top-down.
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out
}

// ParentPath returns the parent directory portion of a wire name: "" for "."
// and empty names, the text before the last slash otherwise, and "." for
// top-level names (oc-rsync parent_path).
func ParentPath(name string) string {
	if name == "." || name == "" {
		return ""
	}
	for i := len(name) - 1; i >= 0; i-- {
		if name[i] == '/' {
			if i == 0 {
				return "."
			}
			return name[:i]
		}
	}
	return "."
}
