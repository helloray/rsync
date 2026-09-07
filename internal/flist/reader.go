package flist

import (
	"io"
)

// Segment is a contiguous run of file-list entries.
//
// For a non-incremental (complete) file list there is a single segment holding
// every entry. Under incremental recursion (protocol >= 30) the transfer is
// broken into one segment per directory subtree, and the reader yields them in
// transmission order.
type Segment struct {
	// NdxStart is the file-list index of the first entry in Entries. The
	// receiver uses the running index (NdxStart + position within Entries) to
	// correlate the file with the NDX codes the sender emits for per-file
	// status messages.
	NdxStart int64
	Entries  []*FileEntry

	// UidNames/GidNames carry the uid/gid name tables transmitted after the
	// entries (a trailing id list). They are non-empty only on the final
	// segment of a complete list, and only when the protocol uses trailing
	// lists (protocol < 30, or without inline names).
	UidNames map[int32]string
	GidNames map[int32]string

	// IOError is the trailing i/o error word, meaningful when EOF is set.
	IOError int32

	// EOF marks the final segment: after it the reader has consumed the id
	// lists and the i/o error word (where the protocol carries them). A
	// subsequent call to Reader.Next returns io.EOF.
	EOF bool
}

// Reader yields file-list segments in transmission order. A Reader is created
// for one file-list transfer and must be drained fully (until Next returns
// io.EOF).
//
// The concrete implementation is swappable without touching the caller: the
// complete reader below materializes the whole list in a single segment, while
// a later incremental reader yields one segment per directory subtree.
type Reader interface {
	Next() (*Segment, error)
}

// completeReader materializes an entire non-incremental file list as a single
// segment. It mirrors the pre-incremental behavior of the receiver: every
// entry, then the trailing id lists, then the i/o error word.
type completeReader struct {
	r      io.Reader
	params Params
	done   bool
}

// NewCompleteReader returns a Reader over a complete, non-incremental file
// list on r, decoded under params.
func NewCompleteReader(r io.Reader, params Params) Reader {
	return &completeReader{r: r, params: params}
}

// Next returns the single segment holding the whole list, or io.EOF once it
// has been consumed.
func (c *completeReader) Next() (*Segment, error) {
	if c.done {
		return nil, io.EOF
	}
	c.done = true
	uid := map[int32]string{}
	gid := map[int32]string{}
	entries, ioErr, err := ReadFileList(c.r, c.params, uid, gid)
	if err != nil {
		return nil, err
	}
	return &Segment{
		NdxStart: 0,
		Entries:  entries,
		UidNames: uid,
		GidNames: gid,
		IOError:  ioErr,
		EOF:      true,
	}, nil
}