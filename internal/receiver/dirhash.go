package receiver

// FNV-1a 64-bit parameters (hash/fnv keeps them unexported).
const (
	fnvOffset64 uint64 = 0xcbf29ce484222325
	fnvPrime64  uint64 = 0x100000001b3
)

// dirHash128 returns a deterministic 128-bit hash of a directory name.
//
// The incremental receiver keeps only these hashes instead of the full
// []*File directory list (docs/dirs-two-phase.md): the segment-validation
// consumer needs to prove that an arriving entry's parent name equals the
// name the parent directory was announced with, not the name itself. Two
// 64-bit halves keep the collision probability negligible (~10⁻²⁷ at 10⁶
// directories); a collision could only let a mis-addressed segment through,
// which the regular protocol flow would then reject.
//
// The hash is a dual-pass FNV-1a: the first half runs forward, the second
// half runs over the reversed bytes with a perturbed offset basis, so
// hierarchical siblings ("a/b" vs "a/c") differ in both passes. It is
// deterministic across processes to keep flist debug sessions reproducible.
func dirHash128(name string) [2]uint64 {
	h1 := fnvOffset64
	h2 := fnvOffset64 ^ fnvPrime64
	for i := 0; i < len(name); i++ {
		h1 = (h1 ^ uint64(name[i])) * fnvPrime64
	}
	for i := len(name) - 1; i >= 0; i-- {
		h2 = (h2 ^ uint64(name[i])) * fnvPrime64
	}
	return [2]uint64{h1, h2}
}
