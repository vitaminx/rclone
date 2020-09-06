package vfs

import (
	"context"
	"os"
	"path"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pkg/errors"
	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/log"
	"github.com/rclone/rclone/fs/operations"
	"github.com/rclone/rclone/vfs/vfscommon"
)

// The File object is tightly coupled to the Dir object. Since they
// both have locks there is plenty of potential for deadlocks. In
// order to mitigate this, we use the following conventions
//
// File may **only** call these methods from Dir with the File lock
// held.
//
//     Dir.Fs
//     Dir.VFS
//
// (As these are read only and do not need to take the Dir mutex.)
//
// File may **not** call any other Dir methods with the File lock
// held. This preserves total lock ordering and makes File subordinate
// to Dir as far as locking is concerned, preventing deadlocks.
//
// File may **not** read any members of Dir directly.

// File represents a file
type File struct {
	inode uint64 // inode number - read only
	size  int64  // size of file - read and written with atomic int64 - must be 64 bit aligned

	mu                sync.RWMutex                    // protects the following
	d                 *Dir                            // parent directory
	dPath             string                          // path of parent directory. NB dir rename means all Files are flushed
	o                 fs.Object                       // NB o may be nil if file is being written
	leaf              string                          // leaf name of the object
	rwOpenCount       int                             // number of open files on this handle
	writers           []Handle                        // writers for this file
	nwriters          int32                           // len(writers) which is read/updated with atomic
	readWriters       int                             // how many RWFileHandle are open for writing
	readWriterClosing bool                            // is an RWFileHandle currently cosing?
	modified          bool                            // has the cache file be modified by an RWFileHandle?
	pendingModTime    time.Time                       // will be applied once o becomes available, i.e. after file was written
	pendingRenameFun  func(ctx context.Context) error // will be run/renamed after all writers close
	appendMode        bool                            // file was opened with O_APPEND
	sys               interface{}                     // user defined info to be attached here

	muRW sync.Mutex // synchronize RWFileHandle.openPending(), RWFileHandle.close() and File.Remove
}

// newFile creates a new File
//
// o may be nil
func newFile(d *Dir, dPath string, o fs.Object, leaf string) *File {
	f := &File{
		d:     d,
		dPath: dPath,
		o:     o,
		leaf:  leaf,
		inode: newInode(),
	}
	if o != nil {
		f.size = o.Size()
	}
	return f
}

// String converts it to printable
func (f *File) String() string {
	if f == nil {
		return "<nil *File>"
	}
	return f.Path()
}

// IsFile returns true for File - satisfies Node interface
func (f *File) IsFile() bool {
	return true
}

// IsDir returns false for File - satisfies Node interface
func (f *File) IsDir() bool {
	return false
}

// Mode bits of the file or directory - satisfies Node interface
func (f *File) Mode() (mode os.FileMode) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	mode = f.d.vfs.Opt.FilePerms
	if f.appendMode {
		mode |= os.ModeAppend
	}
	return mode
}

// Name (base) of the directory - satisfies Node interface
func (f *File) Name() (name string) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.leaf
}

// _path returns the full path of the file
// use when lock is held
func (f *File) _path() string {
	return path.Join(f.dPath, f.leaf)
}

// Path returns the full path of the file
func (f *File) Path() string {
	f.mu.RLock()
	dPath, leaf := f.dPath, f.leaf
	f.mu.RUnlock()
	return path.Join(dPath, leaf)
}

// osPath returns the full path of the file in the cache in OS format
func (f *File) osPath() string {
	return f.d.vfs.cache.ToOSPath(f.Path())
}

// Sys returns underlying data source (can be nil) - satisfies Node interface
func (f *File) Sys() interface{} {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.sys
}

// SetSys sets the underlying data source (can be nil) - satisfies Node interface
func (f *File) SetSys(x interface{}) {
	f.mu.Lock()
	f.sys = x
	f.mu.Unlock()
}

// Inode returns the inode number - satisfies Node interface
func (f *File) Inode() uint64 {
	return f.inode
}

// Node returns the Node assocuated with this - satisfies Noder interface
func (f *File) Node() Node {
	return f
}

// applyPendingRename runs a previously set rename operation if there are no
// more remaining writers. Call without lock held.
func (f *File) applyPendingRename() {
	f.mu.RLock()
	fun := f.pendingRenameFun
	writing := f._writingInProgress()
	f.mu.RUnlock()
	if fun == nil || writing {
		return
	}
	fs.Debugf(f.Path(), "Running delayed rename now")
	if err := fun(context.TODO()); err != nil {
		fs.Errorf(f.Path(), "delayed File.Rename error: %v", err)
	}
}

// rename attempts to immediately rename a file if there are no open writers.
// Otherwise it will queue the rename operation on the remote until no writers
// remain.
func (f *File) rename(ctx context.Context, destDir *Dir, newName string) error {
	f.mu.RLock()
	d := f.d
	oldPendingRenameFun := f.pendingRenameFun
	f.mu.RUnlock()

	if features := d.Fs().Features(); features.Move == nil && features.Copy == nil {
		err := errors.Errorf("Fs %q can't rename files (no server side Move or Copy)", d.Fs())
		fs.Errorf(f.Path(), "Dir.Rename error: %v", err)
		return err
	}

	// File.mu is unlocked here to call Dir.Path()
	newPath := path.Join(destDir.Path(), newName)

	renameCall := func(ctx context.Context) error {
		// chain rename calls if any
		if oldPendingRenameFun != nil {
			err := oldPendingRenameFun(ctx)
			if err != nil {
				return err
			}
		}

		f.mu.RLock()
		o := f.o
		f.mu.RUnlock()
		if o == nil {
			return errors.New("Cannot rename: file object is not available")
		}
		if o.Remote() == newPath {
			return nil // no need to rename
		}

		// do the move of the remote object
		dstOverwritten, _ := d.Fs().NewObject(ctx, newPath)
		newObject, err := operations.Move(ctx, d.Fs(), dstOverwritten, newPath, o)
		if err != nil {
			fs.Errorf(f.Path(), "File.Rename error: %v", err)
			return err
		}

		// newObject can be nil here for example if --dry-run
		if newObject == nil {
			err = errors.New("rename failed: nil object returned")
			fs.Errorf(f.Path(), "File.Rename %v", err)
			return err
		}
		// Update the node with the new details
		fs.Debugf(f.Path(), "Updating file with %v %p", newObject, f)
		// f.rename(destDir, newObject)
		f.mu.Lock()
		f.o = newObject
		f.pendingRenameFun = nil
		f.mu.Unlock()
		return nil
	}

	// Rename in the cache if it exists
	if f.d.vfs.cache != nil && f.d.vfs.cache.Exists(f.Path()) {
		if err := f.d.vfs.cache.Rename(f.Path(), newPath); err != nil {
			fs.Infof(f.Path(), "File.Rename failed in Cache: %v", err)
		}
	}

	// rename the file object
	dPath := destDir.Path()
	f.mu.Lock()
	f.d = destDir
	f.dPath = dPath
	f.leaf = newName
	writing := f._writingInProgress()
	f.mu.Unlock()

	if writing {
		fs.Debugf(f.Path(), "File is currently open, delaying rename %p", f)
		f.mu.Lock()
		f.pendingRenameFun = renameCall
		f.mu.Unlock()
		return nil
	}

	return renameCall(ctx)
}

// addWriter adds a write handle to the file
func (f *File) addWriter(h Handle) {
	f.mu.Lock()
	f.writers = append(f.writers, h)
	atomic.AddInt32(&f.nwriters, 1)
	if _, ok := h.(*RWFileHandle); ok {
		f.readWriters++
	}
	f.mu.Unlock()
}

// delWriter removes a write handle from the file
func (f *File) delWriter(h Handle, modifiedCacheFile bool) (lastWriterAndModified bool) {
	f.mu.Lock()
	defer f.applyPendingRename()
	defer f.mu.Unlock()
	var found = -1
	for i := range f.writers {
		if f.writers[i] == h {
			found = i
			break
		}
	}
	if found >= 0 {
		f.writers = append(f.writers[:found], f.writers[found+1:]...)
		atomic.AddInt32(&f.nwriters, -1)
	} else {
		fs.Debugf(f._path(), "File.delWriter couldn't find handle")
	}
	if _, ok := h.(*RWFileHandle); ok {
		f.readWriters--
	}
	f.readWriterClosing = true
	if modifiedCacheFile {
		f.modified = true
	}
	lastWriterAndModified = len(f.writers) == 0 && f.modified
	if lastWriterAndModified {
		f.modified = false
	}
	return
}

// addRWOpen should be called by ReadWriteHandle when they have
// actually opened the file for read or write.
func (f *File) addRWOpen() {
	f.mu.Lock()
	f.rwOpenCount++
	f.mu.Unlock()
}

// delRWOpen should be called by ReadWriteHandle when they have closed
// an actually opene file for read or write.
func (f *File) delRWOpen() {
	f.mu.Lock()
	f.rwOpenCount--
	f.mu.Unlock()
}

// rwOpens returns how many active open ReadWriteHandles there are.
// Note that file handles which are in pending open state aren't
// counted.
func (f *File) rwOpens() int {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.rwOpenCount
}

// finishWriterClose resets the readWriterClosing flag
func (f *File) finishWriterClose() {
	f.mu.Lock()
	f.readWriterClosing = false
	f.mu.Unlock()
	f.applyPendingRename()
}

// activeWriters returns the number of writers on the file
//
// Note that we don't take the mutex here.  If we do then we can get a
// deadlock.
func (f *File) activeWriters() int {
	return int(atomic.LoadInt32(&f.nwriters))
}

// ModTime returns the modified time of the file
//
// if NoModTime is set then it returns the mod time of the directory
func (f *File) ModTime() (modTime time.Time) {
	f.mu.RLock()
	d, o, pendingModTime := f.d, f.o, f.pendingModTime
	f.mu.RUnlock()

	if d.vfs.Opt.NoModTime {
		return d.ModTime()
	}
	if !pendingModTime.IsZero() {
		return pendingModTime
	}
	if o == nil {
		return time.Now()
	}
	return o.ModTime(context.TODO())
}

// nonNegative returns 0 if i is -ve, i otherwise
func nonNegative(i int64) int64 {
	if i >= 0 {
		return i
	}
	return 0
}

// Size of the file
func (f *File) Size() int64 {
	f.mu.RLock()
	defer f.mu.RUnlock()

	// if o is nil it isn't valid yet or there are writers, so return the size so far
	if f._writingInProgress() {
		return atomic.LoadInt64(&f.size)
	}
	return nonNegative(f.o.Size())
}

// SetModTime sets the modtime for the file
func (f *File) SetModTime(modTime time.Time) error {
	if f.d.vfs.Opt.ReadOnly {
		return EROFS
	}
	f.mu.Lock()
	defer f.mu.Unlock()

	f.pendingModTime = modTime

	// Only update the ModTime when there are no writers, setObject will do it
	if !f._writingInProgress() {
		return f._applyPendingModTime()
	}

	// queue up for later, hoping f.o becomes available
	return nil
}

// Apply a pending mod time
// Call with the mutex held
func (f *File) _applyPendingModTime() error {
	if f.pendingModTime.IsZero() {
		return nil
	}

	defer func() { f.pendingModTime = time.Time{} }()

	if f.o == nil {
		return errors.New("Cannot apply ModTime, file object is not available")
	}

	// set the time of the file in the cache
	if f.d.vfs.cache != nil {
		f.d.vfs.cache.SetModTime(f._path(), f.pendingModTime)
	}

	// set the time of the object
	err := f.o.SetModTime(context.TODO(), f.pendingModTime)
	switch err {
	case nil:
		fs.Debugf(f._path(), "File._applyPendingModTime OK")
	case fs.ErrorCantSetModTime, fs.ErrorCantSetModTimeWithoutDelete:
		// do nothing, in order to not break "touch somefile" if it exists already
	default:
		fs.Debugf(f._path(), "File._applyPendingModTime error: %v", err)
		return err
	}

	return nil
}

// _writingInProgress returns true of there are any open writers
// Call with read lock held
func (f *File) _writingInProgress() bool {
	return f.o == nil || len(f.writers) != 0 || f.readWriterClosing
}

// Update the size while writing
func (f *File) setSize(n int64) {
	atomic.StoreInt64(&f.size, n)
}

// Update the object when written and add it to the directory
func (f *File) setObject(o fs.Object) {
	f.mu.Lock()
	f.o = o
	_ = f._applyPendingModTime()
	f.mu.Unlock()

	// Release File.mu before calling Dir method
	f.d.addObject(f)
}

// Update the object but don't update the directory cache - for use by
// the directory cache
func (f *File) setObjectNoUpdate(o fs.Object) {
	f.mu.Lock()
	f.o = o
	f.mu.Unlock()
}

// Get the current fs.Object - may be nil
func (f *File) getObject() fs.Object {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.o
}

// exists returns whether the file exists already
func (f *File) exists() bool {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.o != nil
}

// Wait for f.o to become non nil for a short time returning it or an
// error.  Use when opening a read handle.
//
// Call without the mutex held
func (f *File) waitForValidObject() (o fs.Object, err error) {
	for i := 0; i < 50; i++ {
		f.mu.RLock()
		o = f.o
		nwriters := len(f.writers)
		wclosing := f.readWriterClosing
		f.mu.RUnlock()
		if o != nil {
			return o, nil
		}
		if nwriters == 0 && !wclosing {
			return nil, errors.New("can't open file - writer failed")
		}
		time.Sleep(100 * time.Millisecond)
	}
	return nil, ENOENT
}

// openRead open the file for read
func (f *File) openRead() (fh *ReadFileHandle, err error) {
	// if o is nil it isn't valid yet
	_, err = f.waitForValidObject()
	if err != nil {
		return nil, err
	}
	// fs.Debugf(f.Path(), "File.openRead")

	fh, err = newReadFileHandle(f)
	if err != nil {
		fs.Debugf(f.Path(), "File.openRead failed: %v", err)
		return nil, err
	}
	return fh, nil
}

// openWrite open the file for write
func (f *File) openWrite(flags int) (fh *WriteFileHandle, err error) {
	f.mu.RLock()
	d := f.d
	f.mu.RUnlock()

	if d.vfs.Opt.ReadOnly {
		return nil, EROFS
	}
	// fs.Debugf(f.Path(), "File.openWrite")

	fh, err = newWriteFileHandle(d, f, f.Path(), flags)
	if err != nil {
		fs.Debugf(f.Path(), "File.openWrite failed: %v", err)
		return nil, err
	}
	return fh, nil
}

// openRW open the file for read and write using a temporay file
//
// It uses the open flags passed in.
func (f *File) openRW(flags int) (fh *RWFileHandle, err error) {
	f.mu.RLock()
	d := f.d
	f.mu.RUnlock()

	// FIXME chunked
	if flags&accessModeMask != os.O_RDONLY && d.vfs.Opt.ReadOnly {
		return nil, EROFS
	}
	// fs.Debugf(f.Path(), "File.openRW")

	fh, err = newRWFileHandle(d, f, flags)
	if err != nil {
		fs.Debugf(f.Path(), "File.openRW failed: %v", err)
		return nil, err
	}
	return fh, nil
}

// Sync the file
//
// Note that we don't do anything except return OK
func (f *File) Sync() error {
	return nil
}

// Remove the file
func (f *File) Remove() error {
	f.mu.RLock()
	d := f.d
	f.mu.RUnlock()

	if d.vfs.Opt.ReadOnly {
		return EROFS
	}
	f.muRW.Lock() // muRW must be locked before mu to avoid
	f.mu.Lock()   // deadlock in RWFileHandle.openPending and .close
	if f.o != nil {
		err := f.o.Remove(context.TODO())
		if err != nil {
			fs.Debugf(f._path(), "File.Remove file error: %v", err)
			f.mu.Unlock()
			f.muRW.Unlock()
			return err
		}
	}
	f.mu.Unlock()
	f.muRW.Unlock()

	// Remove the item from the directory listing
	// called with File.mu released
	d.delObject(f.Name())
	// Remove the object from the cache
	if d.vfs.cache != nil {
		d.vfs.cache.Remove(f.Path())
	}
	return nil
}

// RemoveAll the file - same as remove for files
func (f *File) RemoveAll() error {
	return f.Remove()
}

// DirEntry returns the underlying fs.DirEntry - may be nil
func (f *File) DirEntry() (entry fs.DirEntry) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.o
}

// Dir returns the directory this file is in
func (f *File) Dir() *Dir {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.d
}

// VFS returns the instance of the VFS
func (f *File) VFS() *VFS {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.d.vfs
}

// Fs returns the underlying Fs for the file
func (f *File) Fs() fs.Fs {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.d.Fs()
}

// Open a file according to the flags provided
//
//   O_RDONLY open the file read-only.
//   O_WRONLY open the file write-only.
//   O_RDWR   open the file read-write.
//
//   O_APPEND append data to the file when writing.
//   O_CREATE create a new file if none exists.
//   O_EXCL   used with O_CREATE, file must not exist
//   O_SYNC   open for synchronous I/O.
//   O_TRUNC  if possible, truncate file when opene
//
// We ignore O_SYNC and O_EXCL
func (f *File) Open(flags int) (fd Handle, err error) {
	defer log.Trace(f.Path(), "flags=%s", decodeOpenFlags(flags))("fd=%v, err=%v", &fd, &err)
	var (
		write    bool // if set need write support
		read     bool // if set need read support
		rdwrMode = flags & accessModeMask
	)

	// http://pubs.opengroup.org/onlinepubs/7908799/xsh/open.html
	// The result of using O_TRUNC with O_RDONLY is undefined.
	// Linux seems to truncate the file, but we prefer to return EINVAL
	if rdwrMode == os.O_RDONLY && flags&os.O_TRUNC != 0 {
		return nil, EINVAL
	}

	// Figure out the read/write intents
	switch {
	case rdwrMode == os.O_RDONLY:
		read = true
	case rdwrMode == os.O_WRONLY:
		write = true
	case rdwrMode == os.O_RDWR:
		read = true
		write = true
	default:
		fs.Debugf(f.Path(), "Can't figure out how to open with flags: 0x%X", flags)
		return nil, EPERM
	}

	// If append is set then set read to force openRW
	if flags&os.O_APPEND != 0 {
		read = true
		f.mu.Lock()
		f.appendMode = true
		f.mu.Unlock()
	}

	// If truncate is set then set write to force openRW
	if flags&os.O_TRUNC != 0 {
		write = true
	}

	// Open the correct sort of handle
	f.mu.RLock()
	d := f.d
	f.mu.RUnlock()
	CacheMode := d.vfs.Opt.CacheMode
	if CacheMode >= vfscommon.CacheModeMinimal && (d.vfs.cache.Opens(f.Path()) > 0 || d.vfs.cache.Exists(f.Path())) {
		fd, err = f.openRW(flags)
	} else if read && write {
		if CacheMode >= vfscommon.CacheModeMinimal {
			fd, err = f.openRW(flags)
		} else {
			// Open write only and hope the user doesn't
			// want to read.  If they do they will get an
			// EPERM plus an Error log.
			fd, err = f.openWrite(flags)
		}
	} else if write {
		if CacheMode >= vfscommon.CacheModeWrites {
			fd, err = f.openRW(flags)
		} else {
			fd, err = f.openWrite(flags)
		}
	} else if read {
		if CacheMode >= vfscommon.CacheModeFull {
			fd, err = f.openRW(flags)
		} else {
			fd, err = f.openRead()
		}
	} else {
		fs.Debugf(f.Path(), "Can't figure out how to open with flags: 0x%X", flags)
		return nil, EPERM
	}
	// if creating a file, add the file to the directory
	if err == nil && flags&os.O_CREATE != 0 {
		// called without File.mu held
		d.addObject(f)
	}
	return fd, err
}

// Truncate changes the size of the named file.
func (f *File) Truncate(size int64) (err error) {
	// make a copy of fh.writers with the lock held then unlock so
	// we can call other file methods.
	f.mu.Lock()
	writers := make([]Handle, len(f.writers))
	copy(writers, f.writers)
	o := f.o
	f.mu.Unlock()

	// FIXME: handle closing writer

	// If have writers then call truncate for each writer
	if len(writers) != 0 {
		fs.Debugf(f.Path(), "Truncating %d file handles", len(writers))
		for _, h := range writers {
			truncateErr := h.Truncate(size)
			if truncateErr != nil {
				err = truncateErr
			}
		}
		return err
	}

	// If no writers, and size is already correct then all done
	if o.Size() == size {
		return nil
	}

	fs.Debugf(f.Path(), "Truncating file")

	// Otherwise if no writers then truncate the file by opening
	// the file and truncating it.
	flags := os.O_WRONLY
	if size == 0 {
		flags |= os.O_TRUNC
	}
	fh, err := f.Open(flags)
	if err != nil {
		return err
	}
	defer fs.CheckClose(fh, &err)
	if size != 0 {
		return fh.Truncate(size)
	}
	return nil
}
