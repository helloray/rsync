package flist

import (
	"io"
	"sort"

	"github.com/gokrazy/rsync/internal/protocol"
)

// WriteIDList writes a single uid (or gid) name list to w, mirroring
// _c-rsync/uidlist.c:send_one_list. The format is a sequence of (id, name)
// frames where each id uses varint30 framing (int32 at <30, varint at >=30)
// followed by a one-byte name length and the name; the list is terminated by a
// zero id (a bare 0, no name).
//
// Id 0 cannot be carried here: a zero id is the list terminator, so a name for
// id 0 (e.g. root) has to travel via the separate ID0_NAMES channel (uidlist.c
// xmit_id0_names) or inline. Non-zero ids are the norm, so this is rarely hit.
func WriteIDList(w io.Writer, names map[int32]string, protocolVersion int) error {
	// deterministic order for tests: sort by id.
	ids := make([]int32, 0, len(names))
	for id := range names {
		if id == 0 {
			continue // cannot be represented; would terminate the list
		}
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	for _, id := range ids {
		name := names[id]
		if protocolVersion >= 30 {
			if err := protocol.WriteVarint(w, id); err != nil {
				return err
			}
		} else if err := writeInt32(w, id); err != nil {
			return err
		}
		if err := writeByte(w, byte(len(name))); err != nil {
			return err
		}
		if _, err := io.WriteString(w, name); err != nil {
			return err
		}
	}
	// terminator: a bare zero id.
	if protocolVersion >= 30 {
		return protocol.WriteVarint(w, 0)
	}
	return writeInt32(w, 0)
}

// ReadIDList reads a uid/gid name list written by WriteIDList.
func ReadIDList(r io.Reader, protocolVersion int) (map[int32]string, error) {
	out := make(map[int32]string)
	for {
		var id int32
		if protocolVersion >= 30 {
			v, err := protocol.ReadVarint(r)
			if err != nil {
				return nil, err
			}
			id = v
		} else {
			v, err := readInt32(r)
			if err != nil {
				return nil, err
			}
			id = v
		}
		if id == 0 {
			break
		}
		l, err := readByte(r)
		if err != nil {
			return nil, err
		}
		buf := make([]byte, int(l))
		if _, err := io.ReadFull(r, buf); err != nil {
			return nil, err
		}
		out[id] = string(buf)
	}
	return out, nil
}