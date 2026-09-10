package rsyncchecksum

import (
	"crypto/md5"
	"encoding/binary"
	"hash"
	"io"
	"os"

	"github.com/mmcloughlin/md4"
)

func Tag2(s1, s2 uint16) uint16 {
	return (((s1) + (s2)) & 0xFFFF)
}

func Tag(sum uint32) uint16 {
	return Tag2(uint16(sum&0xFFFF), uint16(sum>>16))
}

// SignExtend mirrors how C converts from (signed char) to uint32, i.e. using
// sign extension. get_checksum1 treats the buffer as (signed char*) instead of
// (unsigned char*), which likely was not a conscious choice, but here we are.
//
// This function is exported for use in the rolling checksum in match.go.
func SignExtend(b byte) uint32 {
	val := uint32(b)
	return uint32(int32(val<<24) >> 24)
}

func Checksum1(buf []byte) uint32 {
	bufLen := len(buf)
	var s1, s2 uint32
	var i int

	if bufLen > 4 {
		for i = 0; i < (bufLen - 4); i += 4 {
			s2 += 4*(s1+SignExtend(buf[i])) +
				3*SignExtend(buf[i+1]) +
				2*SignExtend(buf[i+2]) +
				SignExtend(buf[i+3])
			s1 += SignExtend(buf[i+0]) +
				SignExtend(buf[i+1]) +
				SignExtend(buf[i+2]) +
				SignExtend(buf[i+3])
		}
	}
	for ; i < bufLen; i++ {
		s1 += SignExtend(buf[i])
		s2 += s1
	}
	return (s1 & 0xffff) + (s2 << 16)
}

// NewStrong returns the accumulator for the negotiated strong-checksum
// algorithm name ("md5" or "md4") for whole-file digests (C's sum_init /
// file_checksum, checksum.c): plain digest over the data, no seed.
func NewStrong(algo string) hash.Hash {
	if algo == "md5" {
		return md5.New()
	}
	return md4.New()
}

// seedBytes encodes the checksum seed the way C feeds it into md4/md5 digests
// (4 little-endian bytes); C skips the seed entirely when it is zero
// (checksum.c: if (checksum_seed)).
func seedBytes(seed int32) []byte {
	if seed == 0 {
		return nil
	}
	var b [4]byte
	binary.LittleEndian.PutUint32(b[:], uint32(seed))
	return b[:]
}

// Checksum2 computes the per-block strong checksum the receiver sends back to
// the sender (C's get_checksum2, checksum.c:322). md4 folds the seed in after
// the data; md5 appends it after the data, or prepends it when the peer
// negotiated the seed-order fix (CF_CHKSUM_SEED_FIX, proper_seed_order).
func Checksum2(algo string, properSeedOrder bool, seed int32, buf []byte) []byte {
	sb := seedBytes(seed)
	if algo == "md5" {
		h := md5.New()
		if properSeedOrder {
			if sb != nil {
				h.Write(sb)
			}
			h.Write(buf)
		} else {
			h.Write(buf)
			if sb != nil {
				h.Write(sb)
			}
		}
		return h.Sum(nil)
	}
	h := md4.New()
	h.Write(buf)
	if sb != nil {
		h.Write(sb)
	}
	return h.Sum(nil)
}

func ReaderChecksum(algo string, r io.Reader) ([]byte, error) {
	h := NewStrong(algo)
	if _, err := io.Copy(h, r); err != nil {
		return nil, err
	}
	return h.Sum(nil), nil
}

func RootChecksum(algo string, root interface{ Open(name string) (*os.File, error) }, fn string) ([]byte, error) {
	f, err := root.Open(fn)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return ReaderChecksum(algo, f)
}

// Size is the digest length of both supported strong checksums (md4 and md5
// are both 16 bytes, like C's MAX_DIGEST_LEN for these algorithms).
const Size = md4.Size
