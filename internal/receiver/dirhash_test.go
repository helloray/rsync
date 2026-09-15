package receiver

import (
	"fmt"
	"hash/fnv"
	"testing"

	"github.com/gokrazy/rsync/internal/flist"
)

// TestDirHash128KnownVectors pins the forward pass against published FNV-1a
// 64-bit test vectors and the reverse pass against its definition (same
// FNV-1a round function, offset basis perturbed by the prime, bytes in
// reverse order).
func TestDirHash128KnownVectors(t *testing.T) {
	const offset64 uint64 = 0xcbf29ce484222325
	const prime64 uint64 = 0x100000001b3
	vectors := []struct {
		name string
		h1   uint64 // published FNV-1a 64-bit vectors
	}{
		{"", 0xcbf29ce484222325},
		{"a", 0xaf63dc4c8601ec8c},
		{"foobar", 0x85944171f73967e8},
	}
	for _, v := range vectors {
		got := dirHash128(v.name)
		if got[0] != v.h1 {
			t.Errorf("dirHash128(%q)[0] = %#x, want %#x", v.name, got[0], v.h1)
		}
	}
	// The empty name's reverse pass is the perturbed basis by definition.
	if got := dirHash128("")[1]; got != offset64^prime64 {
		t.Errorf("dirHash128(%q)[1] = %#x, want %#x", "", got, offset64^prime64)
	}

	// Single-character names: one reversed round, independently computed.
	for _, name := range []string{"a", "b", "."} {
		want := (offset64 ^ prime64 ^ uint64(name[0])) * prime64
		if got := dirHash128(name)[1]; got != want {
			t.Errorf("dirHash128(%q)[1] = %#x, want %#x", name, got, want)
		}
	}
}

// TestDirHash128ForwardMatchesStdlib cross-checks the forward half against
// the standard library's FNV-1a for a batch of realistic flist names.
func TestDirHash128ForwardMatchesStdlib(t *testing.T) {
	for _, name := range []string{
		".", "d00", "d00/f00.txt", "a", "a-x", "a.x", "a/b/c/d/e",
		"d0042/sub1/g07.txt", "OpenWrt-SDK-23.05/staging_dir/toolchain",
	} {
		h := fnv.New64a()
		if _, err := h.Write([]byte(name)); err != nil {
			t.Fatal(err)
		}
		if got := dirHash128(name)[0]; got != h.Sum64() {
			t.Errorf("dirHash128(%q)[0] = %#x, want stdlib FNV-1a %#x", name, got, h.Sum64())
		}
	}
}

// TestDirHash128NoCollisions runs a collision sanity check over path-shaped
// names: FNameCmp boundary siblings, hierarchical prefixes, and a
// deterministic pseudo-random batch — the names the segment validation
// actually compares.
func TestDirHash128NoCollisions(t *testing.T) {
	seen := make(map[[2]uint64]string)
	add := func(name string) {
		h := dirHash128(name)
		if prev, ok := seen[h]; ok {
			t.Fatalf("collision: %q and %q hash to %#x", prev, name, h)
		}
		seen[h] = name
	}
	add("")
	add(".")
	// FNameCmp boundary names (see createDeepTree) and hierarchical prefixes.
	add("a")
	add("a-x")
	add("a.x")
	add("a-x.txt")
	add("a/w")
	add("a/x")
	add("a/b")
	add("a/b/c")
	add("ab")
	// A deterministic LCG batch of path-shaped names.
	state := uint64(0x9e3779b97f4a7c15)
	for i := 0; i < 100000; i++ {
		state = state*6364136223846793005 + 1442695040888963407
		add(fmt.Sprintf("d%06d/sub%03d/f%03d.dir", state%1000000, (state>>16)%1000, (state>>32)%1000))
	}
}

// TestDirHash128ParentContract pins the interplay the segment validation
// relies on: hashing an entry's ParentPath yields the same hash as hashing
// the parent directory's own name.
func TestDirHash128ParentContract(t *testing.T) {
	for _, dir := range []string{".", "a", "a/b", "d0042/sub1"} {
		child := dir + "/f.txt"
		if dir == "." {
			child = "f.txt"
		}
		if dirHash128(flist.ParentPath(child)) != dirHash128(dir) {
			t.Errorf("ParentPath(%q) = %q hashes differently than the dir name", child, flist.ParentPath(child))
		}
	}
}
