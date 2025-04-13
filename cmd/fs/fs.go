package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"os"
	"os/signal"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	"bazil.org/fuse"
	fspkg "bazil.org/fuse/fs"
	"github.com/maxmcd/tulip/stargz"
	"golang.org/x/sys/unix"
)

const (
	debug = false

	// whiteoutPrefix is a filename prefix for a "whiteout" file which is an empty
	// file that signifies a path should be deleted.
	// See https://github.com/opencontainers/image-spec/blob/775207bd45b6cb8153ce218cc59351799217451f/layer.md#whiteouts
	whiteoutPrefix = ".wh."

	// whiteoutOpaqueDir is a filename of "opaque whiteout" which indicates that
	// all siblings are hidden in the lower layer.
	// See https://github.com/opencontainers/image-spec/blob/775207bd45b6cb8153ce218cc59351799217451f/layer.md#opaque-whiteout
	whiteoutOpaqueDir = whiteoutPrefix + whiteoutPrefix + ".opq"

	// opaqueXattr is a key of an xattr for an overalyfs opaque directory.
	// See https://www.kernel.org/doc/Documentation/filesystems/overlayfs.txt
	opaqueXattr = "trusted.overlay.opaque"

	// opaqueXattrValue is value of an xattr for an overalyfs opaque directory.
	// See https://www.kernel.org/doc/Documentation/filesystems/overlayfs.txt
	opaqueXattrValue = "y"
)

var (
	fuseDebug = flag.Bool("fuse_debug", false, "enable verbose FUSE debugging")
)

func usage() {
	fmt.Fprintf(os.Stderr, "Usage of %s:\n", os.Args[0])
	fmt.Fprintf(os.Stderr, "   %s <MOUNT_POINT>  (defaults to /crfs)\n", os.Args[0])
	flag.PrintDefaults()
}

func main() {
	flag.Parse()
	mntPoint := "/crfs"
	if flag.NArg() > 1 {
		usage()
		os.Exit(2)
	}
	if flag.NArg() == 1 {
		mntPoint = flag.Arg(0)
	}
	if *fuseDebug {
		fuse.Debug = func(msg interface{}) {
			log.Printf("fuse debug: %v", msg)
		}
	}

	log.Printf("crfs: mounting")
	c, err := fuse.Mount(mntPoint, fuse.FSName("crfs"), fuse.Subtype("crfs"), fuse.ReadOnly(), fuse.AllowOther())
	if err != nil {
		log.Fatal(err)
	}
	defer c.Close()
	defer func() { _ = fuse.Unmount(mntPoint) }()

	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-ch
		log.Println("crfs: unmounting")
		_ = fuse.Unmount(mntPoint)
		c.Close()
		os.Exit(0)
	}()

	log.Printf("crfs: serving")
	fs := new(FS)
	err = fspkg.Serve(c, fs)
	if err != nil {
		log.Fatal(err)
	}

	// check if the mount process has an error to report
	<-c.Ready
	if err := c.MountError; err != nil {
		log.Fatal(err)
	}
}

// FS is the CRFS filesystem.
// It implements https://godoc.org/bazil.org/fuse/fs#FS
type FS struct {
	// TODO: options, probably. logger, etc.
}

// Root returns the root filesystem node for the CRFS filesystem.
// See https://godoc.org/bazil.org/fuse/fs#FS
func (fs *FS) Root() (fspkg.Node, error) {
	return &rootNode{
		fs: fs,
		dirEnts: dirEnts{initChildren: func(de *dirEnts) {
			de.m["rootfs"] = &dirEnt{
				dtype: fuse.DT_Dir,
				lookupNode: func(inode uint64) (fspkg.Node, error) {
					dr := &layerDebugRoot{fs: fs, inode: inode}
					return dr.Lookup(context.Background(), "busybox.stargz")
				},
			}
			de.m["README-crfs.txt"] = &dirEnt{
				dtype: fuse.DT_File,
				lookupNode: func(inode uint64) (fspkg.Node, error) {
					return &staticFile{
						inode:    inode,
						contents: "This is CRFS. See https://github.com/google/crfs.\n",
					}, nil
				},
			}
		}},
	}, nil
}

type dirEnt struct {
	lazyInode
	dtype      fuse.DirentType
	lookupNode func(inode uint64) (fspkg.Node, error)
}

type dirEnts struct {
	initOnce     sync.Once
	initChildren func(*dirEnts)
	mu           sync.Mutex
	m            map[string]*dirEnt
}

func (de *dirEnts) Lookup(ctx context.Context, name string) (fspkg.Node, error) {
	fmt.Println("Lookup", name)
	de.condInit()
	de.mu.Lock()
	defer de.mu.Unlock()
	e, ok := de.m[name]
	if !ok {
		log.Printf("returning ENOENT for name %q", name)
		return nil, fuse.ENOENT
	}
	if e.lookupNode == nil {
		log.Printf("node %q has no lookupNode defined", name)
		return nil, fuse.ENOENT
	}
	return e.lookupNode(e.inode())
}

func (de *dirEnts) ReadDirAll(ctx context.Context) (ents []fuse.Dirent, err error) {
	de.condInit()
	de.mu.Lock()
	defer de.mu.Unlock()
	ents = make([]fuse.Dirent, 0, len(de.m))
	for name, e := range de.m {
		ents = append(ents, fuse.Dirent{
			Name:  name,
			Inode: e.inode(),
			Type:  e.dtype,
		})
	}
	sort.Slice(ents, func(i, j int) bool { return ents[i].Name < ents[j].Name })
	return ents, nil
}

func (de *dirEnts) condInit() { de.initOnce.Do(de.doInit) }
func (de *dirEnts) doInit() {
	de.m = map[string]*dirEnt{}
	if de.initChildren != nil {
		de.initChildren(de)
	}
}

// atomicInodeIncr holds the most previously allocate global inode number.
// It should only be accessed/incremented with sync/atomic.
var atomicInodeIncr uint32

// lazyInode is a lazily-allocated inode number.
//
// We only use 32 bits out of 64 to leave room for overlayfs to play
// games with the upper bits. TODO: maybe that's not necessary.
type lazyInode struct{ v uint32 }

func (si *lazyInode) inode() uint64 {
	for {
		v := atomic.LoadUint32(&si.v)
		if v != 0 {
			return uint64(v)
		}
		v = atomic.AddUint32(&atomicInodeIncr, 1)
		if atomic.CompareAndSwapUint32(&si.v, 0, v) {
			return uint64(v)
		}
	}
}

// rootNode is the contents of /crfs.
// Children include:
//
//	layers/ -- individual layers; directories by hostname/user/layer
//	images/ -- merged layers; directories by hostname/user/layer
//	README-crfs.txt
type rootNode struct {
	fs *FS
	dirEnts
	lazyInode
}

func (n *rootNode) Attr(ctx context.Context, a *fuse.Attr) error {
	setDirAttr(a)
	a.Inode = n.inode()
	a.Valid = 30 * 24 * time.Hour
	return nil
}

func setDirAttr(a *fuse.Attr) {
	a.Mode = 0755 | os.ModeDir
	// TODO: more?
}

// layerDebugRoot is /crfs/layers/local/
// Its contents are *.star.gz files in the current directory.
type layerDebugRoot struct {
	fs    *FS
	inode uint64
}

func (n *layerDebugRoot) Attr(ctx context.Context, a *fuse.Attr) error {
	setDirAttr(a)
	a.Inode = n.inode
	return nil
}

func (n *layerDebugRoot) ReadDirAll(ctx context.Context) (ents []fuse.Dirent, err error) {
	fis, err := ioutil.ReadDir(".")
	for _, fi := range fis {
		name := fi.Name()
		if !strings.HasSuffix(name, ".stargz") {
			continue
		}
		// TODO: populate inode number
		ents = append(ents, fuse.Dirent{Type: fuse.DT_Dir, Name: name})
	}
	return ents, err
}

func (n *layerDebugRoot) Lookup(ctx context.Context, name string) (fspkg.Node, error) {
	f, err := os.Open(name)
	if err != nil {
		return nil, err
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	r, err := stargz.Open(io.NewSectionReader(f, 0, fi.Size()))
	if err != nil {
		f.Close()
		log.Printf("error opening local stargz: %v", err)
		return nil, err
	}
	root, ok := r.Lookup("")
	if !ok {
		f.Close()
		return nil, errors.New("failed to find root in stargz")
	}
	return &node{
		fs:    n.fs,
		te:    root,
		sr:    r,
		f:     f,
		child: make(map[string]fspkg.Node),
	}, nil
}

type staticFile struct {
	contents string
	inode    uint64
}

func (f *staticFile) Attr(ctx context.Context, a *fuse.Attr) error {
	a.Mode = 0644
	a.Inode = f.inode
	a.Size = uint64(len(f.contents))
	a.Blocks = blocksOf(a.Size)
	return nil
}

func (f *staticFile) Read(ctx context.Context, req *fuse.ReadRequest, resp *fuse.ReadResponse) error {
	if req.Offset < 0 {
		return syscall.EINVAL
	}
	if req.Offset > int64(len(f.contents)) {
		resp.Data = nil
		return nil
	}
	bufSize := int64(req.Size)
	remain := int64(len(f.contents)) - req.Offset
	if bufSize > remain {
		bufSize = remain
	}
	resp.Data = make([]byte, bufSize)
	n := copy(resp.Data, f.contents[req.Offset:])
	resp.Data = resp.Data[:n] // redundant, but for clarity
	return nil
}

func inodeOfEnt(ent *stargz.TOCEntry) uint64 {
	return uint64(uintptr(unsafe.Pointer(ent)))
}

func direntType(ent *stargz.TOCEntry) fuse.DirentType {
	switch ent.Type {
	case "dir":
		return fuse.DT_Dir
	case "reg":
		return fuse.DT_File
	case "symlink":
		return fuse.DT_Link
	case "block":
		return fuse.DT_Block
	case "char":
		return fuse.DT_Char
	case "fifo":
		return fuse.DT_FIFO
	}
	return fuse.DT_Unknown
}

// node is a CRFS node in the FUSE filesystem.
// See https://godoc.org/bazil.org/fuse/fs#Node
type node struct {
	fs     *FS
	te     *stargz.TOCEntry
	sr     *stargz.Reader
	f      *os.File // non-nil if root & in debug mode
	opaque bool     // true if this node is an overlayfs opaque directory

	mu sync.Mutex // guards child, below
	// child maps from previously-looked up base names (like "foo.txt") to the
	// fspkg.Node that was previously returned. This prevents FUSE inode numbers
	// from getting out of sync
	child map[string]fspkg.Node
}

var (
	_ fspkg.Node               = (*node)(nil)
	_ fspkg.NodeStringLookuper = (*node)(nil)
	_ fspkg.NodeReadlinker     = (*node)(nil)
	_ fspkg.NodeOpener         = (*node)(nil)
	// TODO: implement NodeReleaser and n.f.Close() when n.f is non-nil

	_ fspkg.HandleReadDirAller = (*nodeHandle)(nil)
	_ fspkg.HandleReader       = (*nodeHandle)(nil)

	_ fspkg.HandleReadDirAller = (*rootNode)(nil)
	_ fspkg.NodeStringLookuper = (*rootNode)(nil)
)

func blocksOf(size uint64) (blocks uint64) {
	blocks = size / 512
	if size%512 > 0 {
		blocks++
	}
	return
}

// Attr populates a with the attributes of n.
// See https://godoc.org/bazil.org/fuse/fs#Node
func (n *node) Attr(ctx context.Context, a *fuse.Attr) error {
	fi := n.te.Stat()
	a.Valid = 30 * 24 * time.Hour
	a.Inode = inodeOfEnt(n.te)
	a.Size = uint64(fi.Size())
	a.Blocks = blocksOf(a.Size)
	a.Mtime = fi.ModTime()
	a.Mode = fi.Mode()
	a.Uid = uint32(n.te.Uid)
	a.Gid = uint32(n.te.Gid)
	a.Rdev = uint32(unix.Mkdev(uint32(n.te.DevMajor), uint32(n.te.DevMinor)))
	a.Nlink = uint32(n.te.NumLink)
	if a.Nlink == 0 {
		a.Nlink = 1 // zero "NumLink" means one so we map them here.
	}
	if debug {
		log.Printf("attr of %s: %s", n.te.Name, *a)
	}
	return nil
}

// ReadDirAll returns all directory entries in the directory node n.
//
// https://godoc.org/bazil.org/fuse/fs#HandleReadDirAller
func (h *nodeHandle) ReadDirAll(ctx context.Context) (ents []fuse.Dirent, err error) {
	n := h.n
	whiteouts := map[string]*stargz.TOCEntry{}
	normalEnts := map[string]bool{}
	n.te.ForeachChild(func(baseName string, ent *stargz.TOCEntry) bool {
		// We don't want to show ".wh."-prefixed whiteout files.
		if strings.HasPrefix(baseName, whiteoutPrefix) {
			if baseName == whiteoutOpaqueDir {
				return true
			}
			// Add an overlayfs-styled whiteout direntry later.
			whiteouts[baseName] = ent
			return true
		}

		normalEnts[baseName] = true
		ents = append(ents, fuse.Dirent{
			Inode: inodeOfEnt(ent),
			Type:  direntType(ent),
			Name:  baseName,
		})
		return true
	})

	// Append whiteouts if no entry replaces the target entry in the lower layer.
	for w, ent := range whiteouts {
		if ok := normalEnts[w[len(whiteoutPrefix):]]; !ok {
			ents = append(ents, fuse.Dirent{
				Inode: inodeOfEnt(ent),
				Type:  fuse.DT_Char,
				Name:  w[len(whiteoutPrefix):],
			})

		}
	}
	sort.Slice(ents, func(i, j int) bool { return ents[i].Name < ents[j].Name })
	return ents, nil
}

// Lookup looks up a child entry of the directory node n.
//
// See https://godoc.org/bazil.org/fuse/fs#NodeStringLookuper
func (n *node) Lookup(ctx context.Context, name string) (fspkg.Node, error) {
	fmt.Println("node.Lookup", name)
	n.mu.Lock()
	defer n.mu.Unlock()
	if c, ok := n.child[name]; ok {
		fmt.Println("node.Lookup", "cached")
		return c, nil
	}

	// We don't want to show ".wh."-prefixed whiteout files.
	if strings.HasPrefix(name, whiteoutPrefix) {
		fmt.Println("whiteout prefix", name)
		return nil, fuse.ENOENT
	}

	e, ok := n.te.LookupChild(name)
	if !ok {
		// If the entry exists as a whiteout, show an overlayfs-styled whiteout node.
		if e, ok := n.te.LookupChild(fmt.Sprintf("%s%s", whiteoutPrefix, name)); ok {
			c := &whiteout{e}
			n.child[name] = c
			return c, nil
		}
		fmt.Println("node.Lookup", name, "returning nil", fuse.ENOENT)
		return nil, fuse.ENOENT
	}

	var opaque bool
	if _, ok := e.LookupChild(whiteoutOpaqueDir); ok {
		// This entry is an opaque directory.
		opaque = true
		fmt.Println("node.Lookup", name, "opaque")
	}

	c := &node{
		fs:     n.fs,
		te:     e,
		sr:     n.sr,
		child:  make(map[string]fspkg.Node),
		opaque: opaque,
	}
	n.child[name] = c
	fmt.Println("node.Lookup", name, "returning", c)
	return c, nil
}

// Readlink reads the target of a symlink.
//
// See https://godoc.org/bazil.org/fuse/fs#NodeReadlinker
func (n *node) Readlink(ctx context.Context, req *fuse.ReadlinkRequest) (string, error) {
	if n.te.Type != "symlink" {
		return "", syscall.EINVAL
	}
	return n.te.LinkName, nil
}

// Listxattr lists the extended attributes specified for the node.
//
// See https://godoc.org/bazil.org/fuse/fs#NodeListxattrer
func (n *node) Listxattr(ctx context.Context, req *fuse.ListxattrRequest, resp *fuse.ListxattrResponse) error {
	var allXattrs []byte
	if n.opaque {
		// This node is an opaque directory so add overlayfs-compliant indicator.
		allXattrs = append(append(allXattrs, []byte(opaqueXattr)...), 0)
	}
	for k := range n.te.Xattrs {
		allXattrs = append(append(allXattrs, []byte(k)...), 0)
	}

	if req.Position >= uint32(len(allXattrs)) {
		resp.Xattr = []byte{}
		return nil
	}
	resp.Xattr = allXattrs[req.Position:]
	return nil
}

// Getxattr reads the specified extended attribute.
//
// See https://godoc.org/bazil.org/fuse/fs#NodeGetxattrer
func (n *node) Getxattr(ctx context.Context, req *fuse.GetxattrRequest, resp *fuse.GetxattrResponse) error {
	var xattr []byte
	if req.Name == opaqueXattr {
		if n.opaque {
			// This node is an opaque directory so give overlayfs-compliant indicator.
			xattr = []byte(opaqueXattrValue)
		}
	} else {
		xattr = n.te.Xattrs[req.Name]
	}
	if req.Position >= uint32(len(xattr)) {
		resp.Xattr = []byte{}
		return nil
	}
	resp.Xattr = xattr[req.Position:]
	return nil
}

func (n *node) Open(ctx context.Context, req *fuse.OpenRequest, resp *fuse.OpenResponse) (fspkg.Handle, error) {
	h := &nodeHandle{
		n:     n,
		isDir: req.Dir,
	}
	resp.Handle = h.HandleID()
	if !req.Dir {
		var err error
		h.sr, err = n.sr.OpenFile(n.te.Name)
		if err != nil {
			return nil, err
		}
	}
	return h, nil
}

// whiteout is an overlayfs whiteout file which is a character device with 0/0
// device number.
// See https://www.kernel.org/doc/Documentation/filesystems/overlayfs.txt
type whiteout struct {
	te *stargz.TOCEntry
}

func (w *whiteout) Attr(ctx context.Context, a *fuse.Attr) error {
	a.Valid = 30 * 24 * time.Hour
	a.Inode = inodeOfEnt(w.te)
	a.Mode = os.ModeDevice | os.ModeCharDevice
	a.Rdev = uint32(unix.Mkdev(0, 0))
	a.Nlink = 1
	return nil
}

// nodeHandle is a node that's been opened (opendir or for read).
type nodeHandle struct {
	n     *node
	isDir bool
	sr    *io.SectionReader // of file bytes

	mu            sync.Mutex
	lastChunkOff  int64
	lastChunkSize int
	lastChunk     []byte
}

func (h *nodeHandle) HandleID() fuse.HandleID {
	return fuse.HandleID(uintptr(unsafe.Pointer(h)))
}

func (h *nodeHandle) chunkData(offset int64, size int) ([]byte, error) {
	h.mu.Lock()
	if h.lastChunkOff == offset && h.lastChunkSize == size {
		defer h.mu.Unlock()
		if debug {
			log.Printf("cache HIT, chunk off=%d/size=%d", offset, size)
		}
		return h.lastChunk, nil
	}
	h.mu.Unlock()

	if debug {
		log.Printf("reading chunk for offset=%d, size=%d", offset, size)
	}
	buf := make([]byte, size)
	n, err := h.sr.ReadAt(buf, offset)
	if debug {
		log.Printf("... ReadAt = %v, %v", n, err)
	}
	if err == nil {
		h.mu.Lock()
		h.lastChunkOff = offset
		h.lastChunkSize = size
		h.lastChunk = buf
		h.mu.Unlock()
	}
	return buf, err
}

// See https://godoc.org/bazil.org/fuse/fs#HandleReader
func (h *nodeHandle) Read(ctx context.Context, req *fuse.ReadRequest, resp *fuse.ReadResponse) error {
	n := h.n

	resp.Data = make([]byte, req.Size)
	nr := 0
	offset := req.Offset
	for nr < req.Size {
		ce, ok := n.sr.ChunkEntryForOffset(n.te.Name, offset+int64(nr))
		if !ok {
			break
		}
		if debug {
			log.Printf("need chunk data for %q at %d (size=%d, for chunk from log %d-%d (%d), phys %d-%d (%d)) ...",
				n.te.Name, req.Offset, req.Size, ce.ChunkOffset, ce.ChunkOffset+ce.ChunkSize, ce.ChunkSize, ce.Offset, ce.NextOffset(), ce.NextOffset()-ce.Offset)
		}
		chunkData, err := h.chunkData(ce.ChunkOffset, int(ce.ChunkSize))
		if err != nil {
			return err
		}
		n := copy(resp.Data[nr:], chunkData[offset+int64(nr)-ce.ChunkOffset:])
		nr += n
	}
	resp.Data = resp.Data[:nr]
	if debug {
		log.Printf("Read response: size=%d @ %d, read %d", req.Size, req.Offset, nr)
	}
	return nil
}
