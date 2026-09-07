package protocol

import "strings"

// This file ports rsync/flist.c:f_name_cmp, the file name comparator used
// to sort file lists for protocol 29 and newer. It orders names so that,
// at each path level, non-directory entries sort before directory
// entries, and a directory’s contents sort directly after the directory.

const (
	fncTypeItem fnamecmpType = iota
	fncTypePath
)

type fnamecmpType int

const (
	fncStateBase fnamecmpState = iota
	fncStateDir
	fncStateSlash
	fncStateTrailing
)

type fnamecmpState int

type fnameCursor struct {
	isDir bool
	base  string
	cur   string
	state fnamecmpState
	typ   fnamecmpType
}

func newFnameCursor(name string, isDir bool) *fnameCursor {
	f := &fnameCursor{isDir: isDir}
	idx := strings.LastIndexByte(name, '/')
	if idx < 0 {
		f.base = name
		if isDir {
			f.typ = fncTypePath
		}
		if f.typ == fncTypePath && name == "." {
			// Like C, a top-level directory named "." compares as an
			// empty t_ITEM.
			f.typ = fncTypeItem
			f.state = fncStateTrailing
		} else {
			f.state = fncStateBase
			f.cur = name
		}
		return f
	}
	f.base = name[idx+1:]
	f.typ = fncTypePath
	f.state = fncStateDir
	f.cur = name[:idx]
	return f
}

// advance switches to the next comparison segment once cur is exhausted,
// mirroring the state transitions in f_name_cmp for the first side.
func (f *fnameCursor) advance() {
	switch f.state {
	case fncStateDir:
		f.state = fncStateSlash
		f.cur = "/"
	case fncStateSlash:
		if f.isDir {
			f.typ = fncTypePath
		} else {
			f.typ = fncTypeItem
		}
		if f.typ == fncTypePath && f.base == "." {
			f.typ = fncTypeItem
			f.state = fncStateTrailing
			f.cur = ""
		} else {
			f.state = fncStateBase
			f.cur = f.base
		}
	case fncStateBase:
		f.state = fncStateTrailing
		if f.typ == fncTypePath {
			f.cur = "/"
		} else {
			f.typ = fncTypeItem
			f.cur = ""
		}
	case fncStateTrailing:
		f.typ = fncTypeItem
		f.cur = ""
	}
}

// FNameCmp implements rsync/flist.c:f_name_cmp for protocol_version >= 29
// (t_PATH semantics). isDir must be computed from the wire file mode
// (mode & S_IFMT == S_IFDIR).
func FNameCmp(name1 string, isDir1 bool, name2 string, isDir2 bool) int {
	c1 := newFnameCursor(name1, isDir1)
	c2 := newFnameCursor(name2, isDir2)

	// rsync/flist.c:f_name_cmp decides by the initial type before any byte
	// comparison: a top-level directory (t_PATH) always sorts after a
	// plain entry (t_ITEM) at the same level, regardless of the byte order
	// of the names ("dummy" sorts before "cheap" because "cheap" is a dir).
	if c1.typ != c2.typ {
		if c1.typ == fncTypePath {
			return 1
		}
		return -1
	}

	for {
		if len(c1.cur) == 0 {
			c1.advance()
			if len(c2.cur) > 0 && c1.typ != c2.typ {
				if c1.typ == fncTypePath {
					return 1
				}
				return -1
			}
		}
		if len(c2.cur) == 0 {
			// The second side’s transitions differ from the first side’s
			// in fncStateBase/fncStateTrailing: they may report equality
			// when the first side is also exhausted.
			switch c2.state {
			case fncStateDir:
				c2.state = fncStateSlash
				c2.cur = "/"
			case fncStateSlash:
				if c2.isDir {
					c2.typ = fncTypePath
				} else {
					c2.typ = fncTypeItem
				}
				if c2.typ == fncTypePath && c2.base == "." {
					c2.typ = fncTypeItem
					c2.state = fncStateTrailing
					c2.cur = ""
				} else {
					c2.state = fncStateBase
					c2.cur = c2.base
				}
			case fncStateBase:
				c2.state = fncStateTrailing
				if c2.typ == fncTypePath {
					c2.cur = "/"
				} else {
					if len(c1.cur) == 0 {
						return 0
					}
					c2.typ = fncTypeItem
					c2.cur = ""
				}
			case fncStateTrailing:
				if len(c1.cur) == 0 {
					return 0
				}
				c2.typ = fncTypeItem
				c2.cur = ""
			}
			if c1.typ != c2.typ {
				if c1.typ == fncTypePath {
					return 1
				}
				return -1
			}
		}

		var b1, b2 byte
		if len(c1.cur) > 0 {
			b1 = c1.cur[0]
			c1.cur = c1.cur[1:]
		}
		if len(c2.cur) > 0 {
			b2 = c2.cur[0]
			c2.cur = c2.cur[1:]
		}
		if dif := int(b1) - int(b2); dif != 0 {
			return dif
		}
	}
}
