package webdav

// FIXME need to fix directory listings reading each file - make an
// override for getcontenttype property?

import (
	"net/http"
	"os"
	"path"
	"strings"

	"github.com/ncw/rclone/cmd"
	"github.com/ncw/rclone/fs"
	"github.com/ncw/rclone/vfs"
	"github.com/ncw/rclone/vfs/vfsflags"
	"github.com/spf13/cobra"
	"golang.org/x/net/context"
	"golang.org/x/net/webdav"
)

// Globals
var (
	bindAddress = "localhost:8081"
)

func init() {
	Command.Flags().StringVarP(&bindAddress, "addr", "", bindAddress, "IPaddress:Port to bind server to.")
	vfsflags.AddFlags(Command.Flags())
}

// Command definition for cobra
var Command = &cobra.Command{
	Use:   "webdav remote:path",
	Short: `Serve remote:path over webdav.`,
	Long: `
rclone serve webdav implements a basic webdav server to serve the
remote over HTTP via the webdav protocol. This can be viewed with a
webdav client or you can make a remote of type webdav to read and
write it.

FIXME at the moment each directory listing reads the start of each
file which is undesirable
`,
	Run: func(command *cobra.Command, args []string) {
		cmd.CheckArgs(1, 1, command, args)
		fsrc := cmd.NewFsSrc(args)
		cmd.Run(false, false, command, func() error {
			return serveWebDav(fsrc)
		})
	},
}

// serve the remote
func serveWebDav(f fs.Fs) error {
	fs.Logf(f, "WebDav Server started on %v", bindAddress)

	webdavFS := &WebDAV{
		f:   f,
		vfs: vfs.New(f, &vfsflags.Opt),
	}

	handler := &webdav.Handler{
		FileSystem: webdavFS,
		LockSystem: webdav.NewMemLS(),
		Logger:     webdavFS.logRequest, // FIXME
	}

	// FIXME use our HTTP transport
	http.Handle("/", handler)
	return http.ListenAndServe(bindAddress, nil)
}

// WebDAV is a webdav.FileSystem interface
//
// A FileSystem implements access to a collection of named files. The elements
// in a file path are separated by slash ('/', U+002F) characters, regardless
// of host operating system convention.
//
// Each method has the same semantics as the os package's function of the same
// name.
//
// Note that the os.Rename documentation says that "OS-specific restrictions
// might apply". In particular, whether or not renaming a file or directory
// overwriting another existing file or directory is an error is OS-dependent.
type WebDAV struct {
	f   fs.Fs
	vfs *vfs.VFS
}

// check interface
var _ webdav.FileSystem = (*WebDAV)(nil)

// logRequest is called by the webdav module on every request
func (w *WebDAV) logRequest(r *http.Request, err error) {
	fs.Infof(r.URL.Path, "%s from %s", r.Method, r.RemoteAddr)
}

// lookup finds the node corresponding to the name
func (w *WebDAV) lookup(name string) (node vfs.Node, err error) {
	name = strings.Trim(name, "/")
	return w.vfs.Lookup(name)
}

// lookupParent finds the parent directory and the leaf name of a path
func (w *WebDAV) lookupParent(name string) (dir *vfs.Dir, leaf string, err error) {
	name = strings.Trim(name, "/")
	parent, leaf := path.Split(name)
	node, err := w.lookup(parent)
	if err != nil {
		return nil, "", err
	}
	if node.IsFile() {
		return nil, "", os.ErrExist
	}
	dir = node.(*vfs.Dir)
	return dir, leaf, nil
}

// Mkdir creates a directory
func (w *WebDAV) Mkdir(ctx context.Context, name string, perm os.FileMode) (err error) {
	defer fs.Trace(name, "perm=%v", perm)("err = %v", &err)
	dir, leaf, err := w.lookupParent(name)
	if err != nil {
		return err
	}
	_, err = dir.Mkdir(leaf)
	return err
}

// OpenFile opens a file or a directory
func (w *WebDAV) OpenFile(ctx context.Context, name string, flags int, perm os.FileMode) (file webdav.File, err error) {
	defer fs.Trace(name, "flags=%v, perm=%v", flags, perm)("err = %v", &err)
	rdwrMode := flags & (os.O_RDONLY | os.O_WRONLY | os.O_RDWR)
	var read bool
	switch {
	case rdwrMode == os.O_RDONLY:
		read = true
	case rdwrMode == os.O_WRONLY || (rdwrMode == os.O_RDWR && (flags&os.O_TRUNC) != 0):
		read = false
	case rdwrMode == os.O_RDWR:
		fs.Errorf(name, "Can't open for Read and Write")
		return nil, os.ErrPermission
	default:
		fs.Errorf(name, "Can't figure out how to open with flags: 0x%X", flags)
		return nil, os.ErrPermission
	}
	node, err := w.lookup(name)
	if err != nil {
		if err == os.ErrNotExist && !read {
			return w.createFile(ctx, name, flags, perm)
		}
		return nil, err
	}
	if node.IsFile() {
		return w.openFile(ctx, name, flags, perm, node.(*vfs.File), read)
	}
	return w.openDir(ctx, name, flags, perm, node.(*vfs.Dir))
}

func (w *WebDAV) createFile(ctx context.Context, name string, flags int, perm os.FileMode) (davfile webdav.File, err error) {
	fs.Debugf(name, "open for create")
	dir, leaf, err := w.lookupParent(name)
	if err != nil {
		return nil, err
	}
	fd := &File{name: name}
	fd.file, fd.wr, err = dir.Create(leaf)
	if err != nil {
		return nil, err
	}
	return fd, nil
}

func (w *WebDAV) openFile(ctx context.Context, name string, flags int, perm os.FileMode, file *vfs.File, read bool) (davfile webdav.File, err error) {
	fs.Debugf(name, "open for read = %v", read)
	fd := &File{name: name, file: file}
	if read {
		fd.rd, err = file.OpenRead()
	} else {
		fd.wr, err = file.OpenWrite()
	}
	if err != nil {
		return nil, err
	}
	return fd, nil
}

func (w *WebDAV) openDir(ctx context.Context, name string, flag int, perm os.FileMode, dir *vfs.Dir) (file webdav.File, err error) {
	return &Dir{name: name, dir: dir}, nil
}

// RemoveAll removes a file or a directory and its contents
func (w *WebDAV) RemoveAll(ctx context.Context, name string) (err error) {
	defer fs.Trace(name, "")("err = %v", &err)
	node, err := w.lookup(name)
	if err != nil {
		return err
	}
	err = node.RemoveAll()
	if err != nil {
		return err
	}
	return nil
}

// Rename a file or a directory
func (w *WebDAV) Rename(ctx context.Context, oldName, newName string) (err error) {
	defer fs.Trace(oldName, "newName=%q", newName)("err = %v", &err)
	// find the parent directories
	oldDir, oldLeaf, err := w.lookupParent(oldName)
	if err != nil {
		return err
	}
	newDir, newLeaf, err := w.lookupParent(newName)
	if err != nil {
		return err
	}
	err = oldDir.Rename(oldLeaf, newLeaf, newDir)
	if err != nil {
		return err
	}
	return nil
}

// Stat returns info about the file or directory
func (w *WebDAV) Stat(ctx context.Context, name string) (fi os.FileInfo, err error) {
	defer fs.Trace(name, "")("fi=%+v, err = %v", &fi, &err)
	node, err := w.lookup(name)
	if err != nil {
		return nil, err
	}
	return node, nil
}

// check interface
var _ os.FileInfo = vfs.Node(nil)

// A File is returned by a FileSystem's OpenFile method and can be served by a
// Handler.
//
// A File may optionally implement the DeadPropsHolder interface, if it can
// load and save dead properties.
//
// This either has rd != nil or wr != nil.
type File struct {
	name string
	file *vfs.File
	rd   *vfs.ReadFileHandle
	wr   *vfs.WriteFileHandle
}

// check interface
var _ webdav.File = (*File)(nil)

// Close the file
func (fd *File) Close() (err error) {
	defer fs.Trace(fd.name, "")("err = %v", &err)
	if fd.wr != nil {
		return fd.wr.Close()
	}
	return fd.rd.Close()
}

// Seek the file
func (fd *File) Seek(offset int64, whence int) (n int64, err error) {
	defer fs.Trace(fd.name, "offset=%v, whence=%v", offset, whence)("n = %v, err = %v", &n, &err)
	if fd.wr != nil {
		fs.Debugf(fd.name, "Can't seek when writing")
		return 0, os.ErrPermission
	}
	return fd.rd.Seek(offset, whence)
}

// Read data from file
func (fd *File) Read(p []byte) (n int, err error) {
	defer fs.Trace(fd.name, "p=%p (size %d)", p, len(p))("n = %v, err = %v", &n, &err)
	if fd.wr != nil {
		fs.Debugf(fd.name, "Can't read from write only file")
		return 0, os.ErrPermission
	}
	n, err = fd.rd.Read(p)
	if err != nil {
		return 0, err
	}
	return n, nil
}

// Readdir returns a directory listing
func (fd *File) Readdir(count int) (fis []os.FileInfo, err error) {
	defer fs.Trace(fd.name, "cound=%v", count)("fis = %p, err = %v", &fis, &err)
	return nil, os.ErrInvalid
}

// Stat shows info about the file
func (fd *File) Stat() (fi os.FileInfo, err error) {
	defer fs.Trace(fd.name, "")("fi = %p, err = %v", &fi, &err)
	return fd.file, nil
}

// Write to the current position
func (fd *File) Write(p []byte) (n int, err error) {
	defer fs.Trace(fd.name, "p = %p, size %d", p, len(p))("n = %p, err = %v", &n, &err)
	if fd.rd != nil {
		fs.Debugf(fd.name, "Can't write to read only file")
		return 0, os.ErrPermission
	}
	n, err = fd.wr.Write(p)
	if err != nil {
		return n, err
	}
	return n, nil
}

// A Dir is returned by a FileSystem's OpenFile method and can be served by a
// Handler.
//
// A Dir may optionally implement the DeadPropsHolder interface, if it can
// load and save dead properties.
type Dir struct {
	name string
	dir  *vfs.Dir
}

// check interface
var _ webdav.File = (*Dir)(nil)

// Close the directory
func (fd *Dir) Close() (err error) {
	defer fs.Trace(fd.name, "")("err = %v", &err)
	return nil
}

// Seek the directory
func (fd *Dir) Seek(offset int64, whence int) (n int64, err error) {
	defer fs.Trace(fd.name, "offset=%v, whence=%v", offset, whence)("n = %v, err = %v", &n, &err)
	return 0, os.ErrInvalid
}

// Read bytes from the directory
func (fd *Dir) Read(p []byte) (n int, err error) {
	defer fs.Trace(fd.name, "p=%p", p)("n = %v, err = %v", &n, &err)
	return 0, os.ErrInvalid
}

// Readdir returns directory entries
func (fd *Dir) Readdir(count int) (fis []os.FileInfo, err error) {
	entries := -1
	defer fs.Trace(fd.name, "count=%v", count)("entries = %d, err = %v", &entries, &err)
	// FIXME do something with count
	items, err := fd.dir.ReadDirAll()
	if err != nil {
		return nil, err
	}
	for _, node := range items {
		fis = append(fis, node)
	}
	entries = len(fis)
	return fis, nil
}

// Stat returns info about the directory
func (fd *Dir) Stat() (fi os.FileInfo, err error) {
	defer fs.Trace(fd.name, "")("fi = %p, err = %v", &fi, &err)
	return fd.dir, nil
}

// Write to the directory
func (fd *Dir) Write(p []byte) (n int, err error) {
	defer fs.Trace(fd.name, "p = %p")("n = %p, err = %v", &n, &err)
	return 0, os.ErrInvalid
}
