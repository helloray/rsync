package receiver

// mapping pairs a remote uid/gid with the local id resolved from its name.
// The receiver's uid/gid name tables (rt.Users/rt.Groups) are carried for
// compatibility: the trailing id lists on the wire are consumed inside
// flist.ReadFileList, and this type models each entry for later use.
type mapping struct {
	Name    string
	LocalId int32
}