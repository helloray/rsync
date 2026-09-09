package sender

import (
	"io/fs"
	"testing"
	"testing/fstest"
)

// newScannerWalker builds a scopedWalker over a MapFS, mirroring how
// SendFileList wires one up per requested path.
func newScannerWalker(t *testing.T, mapfs fstest.MapFS, requested string) *scopedWalker {
	t.Helper()
	st, _ := newGoldenTransfer(t, mapfs)
	return &scopedWalker{
		st:        st,
		excl:      &filterRuleList{},
		uidMap:    map[int32]string{},
		gidMap:    map[int32]string{},
		fileList:  &fileList{},
		source:    st.Source,
		ioError:   func(err error) {},
		requested: requested,
		strip:     getStrip(requested),
		subdir:    ".",
	}
}

// TestScanDirOrder checks the per-segment FNameCmp sort: a sibling file name
// sorting between a dir and its children (dash before slash) and the dir
// tie-break of the comparator, plus the child-dir walk paths in wire order.
func TestScanDirOrder(t *testing.T) {
	sw := newScannerWalker(t, fstest.MapFS{
		"a/x":   {Data: []byte("x")},
		"a/y/z": {Data: []byte("z")},
		"a-x/w": {Data: []byte("w")},
		"top":   {Data: []byte("t")},
	}, "/")

	kids, dirs, err := sw.scanDir(".")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for i := range kids {
		names = append(names, kids[i].Wpath)
	}
	// FNameCmp decides by type before bytes: a directory (t_PATH) sorts
	// after plain entries (t_ITEM) at the same level (flist.c:3594), so
	// "top" and "a-x" precede the dir "a"; among the files byte order wins.
	eq(t, names, []string{"top", "a-x", "a"})
	// both "a" and "a-x" are dirs; they appear in wire order
	if len(dirs) != 2 || dirs[0].dpath != "a-x" || dirs[1].dpath != "a" {
		t.Fatalf("dirs = %+v, want [a-x a]", dirs)
	}
}

// TestScanDirNested checks scanning a subdirectory walk-path: child walk
// paths are prefixed with the parent walk-path, while wire names follow the
// strip mapping.
func TestScanDirNested(t *testing.T) {
	sw := newScannerWalker(t, fstest.MapFS{
		"a/b/f": {Data: []byte("f")},
		"a/b/g": {Data: []byte("g")},
		"a/c":   {Data: []byte("c")},
	}, "/")

	kids, dirs, err := sw.scanDir("a")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for i := range kids {
		names = append(names, kids[i].Wpath)
	}
	// "a/c" is a plain entry, "a/b" a dir (t_PATH): the file sorts first
	eq(t, names, []string{"a/c", "a/b"})
	if len(dirs) != 1 || dirs[0].dpath != "a/b" {
		t.Fatalf("dirs = %+v, want [a/b]", dirs)
	}
}

// TestScanDirExcluded checks that an excluded entry (and, for dirs, its
// subtree) is skipped while the rest of the scan continues.
func TestScanDirExcluded(t *testing.T) {
	sw := newScannerWalker(t, fstest.MapFS{
		"a/x":   {Data: []byte("x")},
		"b/y":   {Data: []byte("y")},
		"b.txt": {Data: []byte("b")},
	}, "/")
	sw.excl.addRule(&filterRule{pattern: "a"})

	kids, dirs, err := sw.scanDir(".")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for i := range kids {
		names = append(names, kids[i].Wpath)
	}
	eq(t, names, []string{"b.txt", "b"})
	if len(dirs) != 1 || dirs[0].dpath != "b" {
		t.Fatalf("dirs = %+v, want [b]", dirs)
	}
}

// TestScanDirNoDirs checks that --no-dirs removes directory entries (and
// their subtrees) from the scan.
func TestScanDirNoDirs(t *testing.T) {
	// without -r, xfer_dirs defaults to 0, so dirs are skipped entirely
	// (with -r, rsync forces xfer_dirs = 1, rsyncopts.go:1567)
	st, _ := newGoldenTransferNoRecurse(t, fstest.MapFS{
		"a/x": {Data: []byte("x")},
		"top": {Data: []byte("t")},
	})

	sw := &scopedWalker{
		st:        st,
		excl:      &filterRuleList{},
		uidMap:    map[int32]string{},
		gidMap:    map[int32]string{},
		fileList:  &fileList{},
		source:    st.Source,
		ioError:   func(err error) {},
		requested: "/",
	}
	kids, dirs, err := sw.scanDir(".")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for i := range kids {
		names = append(names, kids[i].Wpath)
	}
	eq(t, names, []string{"top"})
	if len(dirs) != 0 {
		t.Fatalf("dirs = %+v, want none", dirs)
	}
}

// TestScanRootDotDir checks the dot-dir case: the root entry plus its
// immediate children land in the initial list, and child dirs are returned
// for the scheduler's tree.
func TestScanRootDotDir(t *testing.T) {
	sw := newScannerWalker(t, fstest.MapFS{
		"a/b/f":   {Data: []byte("f")},
		"a/c.txt": {Data: []byte("c")},
		"top.txt": {Data: []byte("t")},
	}, "/")

	initial, dirs, err := sw.scanRoot()
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for i := range initial {
		names = append(names, initial[i].Wpath)
	}
	// root entry "." plus slash-free children; the file "top.txt" sorts
	// before the dir "a" (t_ITEM before t_PATH, flist.c:3594)
	eq(t, names, []string{".", "top.txt", "a"})
	if len(dirs) != 1 || dirs[0].dpath != "a" {
		t.Fatalf("dirs = %+v, want [a]", dirs)
	}
	// deep entry a/b/f must NOT be in the initial list
	for i := range initial {
		if initial[i].Wpath == "a/b/f" {
			t.Fatalf("initial list contains deep entry %q", initial[i].Wpath)
		}
	}
}

// TestScanRootSubdirArg checks the non-dot dir argument: the root entry is
// the only initial entry, and the returned dirLoc names the root itself (its
// children form its own segment later).
func TestScanRootSubdirArg(t *testing.T) {
	sw := newScannerWalker(t, fstest.MapFS{
		"sub/x":   {Data: []byte("x")},
		"sub/y/z": {Data: []byte("z")},
	}, "/sub")

	initial, dirs, err := sw.scanRoot()
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for i := range initial {
		names = append(names, initial[i].Wpath)
	}
	eq(t, names, []string{"sub"})
	if len(dirs) != 1 || dirs[0].dpath != "sub" {
		t.Fatalf("dirs = %+v, want [sub]", dirs)
	}

	// the root's segment scan: children carry the "sub/" prefix
	kids, kidDirs, err := dirs[0].sw.scanDir(dirs[0].dpath)
	if err != nil {
		t.Fatal(err)
	}
	var kidNames []string
	for i := range kids {
		kidNames = append(kidNames, kids[i].Wpath)
	}
	eq(t, kidNames, []string{"sub/x", "sub/y"})
	if len(kidDirs) != 1 || kidDirs[0].dpath != "sub/y" {
		t.Fatalf("kidDirs = %+v, want [sub/y]", kidDirs)
	}
}

// TestScanRootSingleFile checks a single-file request: one entry, no dirs.
func TestScanRootSingleFile(t *testing.T) {
	sw := newScannerWalker(t, fstest.MapFS{
		"just-a-file": {Data: []byte("j")},
	}, "/just-a-file")

	initial, dirs, err := sw.scanRoot()
	if err != nil {
		t.Fatal(err)
	}
	if len(initial) != 1 || initial[0].Wpath != "just-a-file" || initial[0].isDir() {
		t.Fatalf("initial = %+v", initial)
	}
	if len(dirs) != 0 {
		t.Fatalf("dirs = %+v, want none", dirs)
	}
}

// TestScanRootExcluded checks that an excluded transfer scope yields an
// empty initial list and no dirs.
func TestScanRootExcluded(t *testing.T) {
	sw := newScannerWalker(t, fstest.MapFS{
		"sub/x": {Data: []byte("x")},
	}, "/sub")
	sw.excl.addRule(&filterRule{pattern: "sub"})

	initial, dirs, err := sw.scanRoot()
	if err != nil {
		t.Fatal(err)
	}
	if len(initial) != 0 || len(dirs) != 0 {
		t.Fatalf("initial = %+v, dirs = %+v; want empty", initial, dirs)
	}
}

// TestScanRootIoError checks that a stat failure on the transfer root sets
// the i/o error flag and yields an empty list, like walkFn does.
func TestScanRootIoError(t *testing.T) {
	sw := newScannerWalker(t, fstest.MapFS{
		"present": {Data: []byte("p")},
	}, "/missing")
	ioErrored := false
	sw.ioError = func(err error) {
		ioErrored = true
	}
	initial, dirs, err := sw.scanRoot()
	if err != nil {
		t.Fatal(err)
	}
	if !ioErrored {
		t.Error("ioError callback not invoked")
	}
	if len(initial) != 0 || len(dirs) != 0 {
		t.Fatalf("initial = %+v, dirs = %+v; want empty", initial, dirs)
	}
}

// TestScanRootEmptyDirEntry checks a MapFS with only an explicitly empty
// directory: the dot-dir initial list carries "." and the empty dir, and the
// empty dir still produces a segment.
func TestScanRootEmptyDirEntry(t *testing.T) {
	sw := newScannerWalker(t, fstest.MapFS{
		"empty": &fstest.MapFile{Mode: fs.ModeDir},
	}, "/")

	initial, dirs, err := sw.scanRoot()
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for i := range initial {
		names = append(names, initial[i].Wpath)
	}
	eq(t, names, []string{".", "empty"})
	if len(dirs) != 1 || dirs[0].dpath != "empty" {
		t.Fatalf("dirs = %+v, want [empty]", dirs)
	}
}
