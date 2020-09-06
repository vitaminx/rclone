package vfs

import (
	"context"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"runtime"
	"sync"

	"github.com/pkg/errors"
	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/log"
	"github.com/rclone/rclone/lib/file"
)

// RWFileHandle is a handle that can be open for read and write.
//
// It will be open to a temporary file which, when closed, will be
// transferred to the remote.
type RWFileHandle struct {
	mu          sync.Mutex
	fd          *os.File
	offset      int64 // file pointer offset
	file        *File
	d           *Dir
	flags       int  // open flags
	closed      bool // set if handle has been closed
	opened      bool
	writeCalled bool // if any Write() methods have been called
	changed     bool // file contents was changed in any other way
}

func newRWFileHandle(d *Dir, f *File, flags int) (fh *RWFileHandle, err error) {
	// if O_CREATE and O_EXCL are set and if path already exists, then return EEXIST
	if flags&(os.O_CREATE|os.O_EXCL) == os.O_CREATE|os.O_EXCL && f.exists() {
		return nil, EEXIST
	}

	fh = &RWFileHandle{
		file:  f,
		d:     d,
		flags: flags,
	}

	// mark the file as open in the cache - must be done before the mkdir
	fh.d.VFS().cache.Open(fh.file.Path())

	// Make a place for the file
	_, err = d.VFS().cache.Mkdir(fh.file.Path())
	if err != nil {
		fh.d.VFS().cache.Close(fh.file.Path())
		return nil, errors.Wrap(err, "open RW handle failed to make cache directory")
	}

	rdwrMode := fh.flags & accessModeMask
	if rdwrMode != os.O_RDONLY {
		fh.file.addWriter(fh)
	}

	// truncate or create files immediately to prepare the cache
	if fh.flags&os.O_TRUNC != 0 || fh.flags&(os.O_CREATE) != 0 && !f.exists() {
		if err := fh.openPending(false); err != nil {
			fh.file.delWriter(fh, false)
			return nil, err
		}
	}

	return fh, nil
}

// openPending opens the file if there is a pending open
//
// call with the lock held
func (fh *RWFileHandle) openPending(truncate bool) (err error) {
	if fh.opened {
		return nil
	}

	fh.file.muRW.Lock()
	defer fh.file.muRW.Unlock()

	o := fh.file.getObject()

	var fd *os.File
	cacheFileOpenFlags := fh.flags
	// if not truncating the file, need to read it first
	if fh.flags&os.O_TRUNC == 0 && !truncate {
		// If the remote object exists AND its cached file exists locally AND there are no
		// other RW handles with it open, then attempt to update it.
		if o != nil && fh.file.rwOpens() == 0 {
			err = fh.d.VFS().cache.Check(context.TODO(), o, fh.file.Path())
			if err != nil {
				return errors.Wrap(err, "open RW handle failed to check cache file")
			}
		}

		// try to open an existing cache file
		fd, err = file.OpenFile(fh.file.osPath(), cacheFileOpenFlags&^os.O_CREATE, 0600)
		if os.IsNotExist(err) {
			// cache file does not exist, so need to fetch it if we have an object to fetch
			// it from
			if o != nil {
				err = fh.d.VFS().cache.Fetch(context.TODO(), o, fh.file.Path())
				if err != nil {
					cause := errors.Cause(err)
					if cause != fs.ErrorObjectNotFound && cause != fs.ErrorDirNotFound {
						// return any non NotFound errors
						return errors.Wrap(err, "open RW handle failed to cache file")
					}
					// continue here with err=fs.Error{Object,Dir}NotFound
				}
			}
			// if err == nil, then we have cached the file successfully, otherwise err is
			// indicating some kind of non existent file/directory either
			// os.IsNotExist(err) or fs.Error{Object,Dir}NotFound
			if err != nil {
				if fh.flags&os.O_CREATE != 0 {
					// if the object wasn't found AND O_CREATE is set then
					// ignore error as we are about to create the file
					fh.file.setSize(0)
					fh.changed = true
				} else {
					return errors.Wrap(err, "open RW handle failed to cache file")
				}
			}
		} else if err != nil {
			return errors.Wrap(err, "cache open file failed")
		} else {
			fs.Debugf(fh.logPrefix(), "Opened existing cached copy with flags=%s", decodeOpenFlags(fh.flags))
		}
	} else {
		// Set the size to 0 since we are truncating and flag we need to write it back
		fh.file.setSize(0)
		fh.changed = true
		if fh.flags&os.O_CREATE == 0 && fh.file.exists() {
			// create an empty file if it exists on the source
			err = ioutil.WriteFile(fh.file.osPath(), []byte{}, 0600)
			if err != nil {
				return errors.Wrap(err, "cache open failed to create zero length file")
			}
		}
		// Windows doesn't seem to deal well with O_TRUNC and
		// certain access modes so truncate the file if it
		// exists in these cases.
		if runtime.GOOS == "windows" && fh.flags&os.O_APPEND != 0 {
			cacheFileOpenFlags &^= os.O_TRUNC
			_, err = os.Stat(fh.file.osPath())
			if err == nil {
				err = os.Truncate(fh.file.osPath(), 0)
				if err != nil {
					return errors.Wrap(err, "cache open failed to truncate")
				}
			}
		}
	}

	if fd == nil {
		fs.Debugf(fh.logPrefix(), "Opening cached copy with flags=%s", decodeOpenFlags(fh.flags))
		fd, err = file.OpenFile(fh.file.osPath(), cacheFileOpenFlags, 0600)
		if err != nil {
			return errors.Wrap(err, "cache open file failed")
		}
	}
	fh.fd = fd
	fh.opened = true
	fh.file.addRWOpen()
	fh.d.addObject(fh.file) // make sure the directory has this object in it now
	return nil
}

// String converts it to printable
func (fh *RWFileHandle) String() string {
	if fh == nil {
		return "<nil *RWFileHandle>"
	}
	if fh.file == nil {
		return "<nil *RWFileHandle.file>"
	}
	return fh.file.String() + " (rw)"
}

// Node returns the Node assocuated with this - satisfies Noder interface
func (fh *RWFileHandle) Node() Node {
	fh.mu.Lock()
	defer fh.mu.Unlock()
	return fh.file
}

// Returns whether the file needs to be written back.
//
// If write hasn't been called and the file hasn't been changed in any other
// way we haven't modified it so we don't need to transfer it
//
// Must be called with fh.mu held
func (fh *RWFileHandle) modified() bool {
	if !fh.writeCalled && !fh.changed {
		fs.Debugf(fh.logPrefix(), "not modified so not transferring")
		return false
	}
	return true
}

// flushWrites flushes any pending writes to cloud storage
//
// Must be called with fh.muRW held
func (fh *RWFileHandle) flushWrites(closeFile bool) error {
	if fh.closed && !closeFile {
		return nil
	}

	rdwrMode := fh.flags & accessModeMask
	writer := rdwrMode != os.O_RDONLY

	// If read only then return
	if !fh.opened && rdwrMode == os.O_RDONLY {
		return nil
	}

	isCopied := false
	if writer {
		isCopied = fh.file.delWriter(fh, fh.modified())
		defer fh.file.finishWriterClose()
	}

	// If we aren't creating or truncating the file then
	// we haven't modified it so don't need to transfer it
	if fh.flags&(os.O_CREATE|os.O_TRUNC) != 0 {
		if err := fh.openPending(false); err != nil {
			return err
		}
	}

	if writer && fh.opened {
		fi, err := fh.fd.Stat()
		if err != nil {
			fs.Errorf(fh.logPrefix(), "Failed to stat cache file: %v", err)
		} else {
			fh.file.setSize(fi.Size())
		}
	}

	// Close the underlying file
	if fh.opened && closeFile {
		err := fh.fd.Close()
		if err != nil {
			err = errors.Wrap(err, "failed to close cache file")
			return err
		}
	}

	if isCopied {
		o, err := fh.d.VFS().cache.Store(context.TODO(), fh.file.getObject(), fh.file.Path())
		if err != nil {
			fs.Errorf(fh.logPrefix(), "%v", err)
			return err
		}
		fh.file.setObject(o)
		fs.Debugf(o, "transferred to remote")
	}

	return nil
}

// close the file handle returning EBADF if it has been
// closed already.
//
// Must be called with fh.mu held
//
// Note that we leave the file around in the cache on error conditions
// to give the user a chance to recover it.
func (fh *RWFileHandle) close() (err error) {
	defer log.Trace(fh.logPrefix(), "")("err=%v", &err)
	fh.file.muRW.Lock()
	defer fh.file.muRW.Unlock()

	if fh.closed {
		return ECLOSED
	}
	fh.closed = true
	defer func() {
		if fh.opened {
			fh.file.delRWOpen()
		}
		fh.d.VFS().cache.Close(fh.file.Path())
	}()

	return fh.flushWrites(true)
}

// Close closes the file
func (fh *RWFileHandle) Close() error {
	fh.mu.Lock()
	defer fh.mu.Unlock()
	return fh.close()
}

// Flush is called each time the file or directory is closed.
// Because there can be multiple file descriptors referring to a
// single opened file, Flush can be called multiple times.
func (fh *RWFileHandle) Flush() error {
	fh.mu.Lock()
	defer fh.mu.Unlock()
	if !fh.opened {
		return nil
	}
	if fh.closed {
		fs.Debugf(fh.logPrefix(), "RWFileHandle.Flush nothing to do")
		return nil
	}
	// fs.Debugf(fh.logPrefix(), "RWFileHandle.Flush")
	if !fh.opened {
		fs.Debugf(fh.logPrefix(), "RWFileHandle.Flush ignoring flush on unopened handle")
		return nil
	}

	// If Write hasn't been called then ignore the Flush - Release
	// will pick it up
	if !fh.writeCalled {
		fs.Debugf(fh.logPrefix(), "RWFileHandle.Flush ignoring flush on unwritten handle")
		return nil
	}

	fh.file.muRW.Lock()
	defer fh.file.muRW.Unlock()
	err := fh.flushWrites(false)

	if err != nil {
		fs.Errorf(fh.logPrefix(), "RWFileHandle.Flush error: %v", err)
	} else {
		// fs.Debugf(fh.logPrefix(), "RWFileHandle.Flush OK")
	}
	return err
}

// Release is called when we are finished with the file handle
//
// It isn't called directly from userspace so the error is ignored by
// the kernel
func (fh *RWFileHandle) Release() error {
	fh.mu.Lock()
	defer fh.mu.Unlock()
	if fh.closed {
		fs.Debugf(fh.logPrefix(), "RWFileHandle.Release nothing to do")
		return nil
	}
	fs.Debugf(fh.logPrefix(), "RWFileHandle.Release closing")
	err := fh.close()
	if err != nil {
		fs.Errorf(fh.logPrefix(), "RWFileHandle.Release error: %v", err)
	} else {
		// fs.Debugf(fh.logPrefix(), "RWFileHandle.Release OK")
	}
	return err
}

// _size returns the size of the underlying file
//
// call with the lock held
//
// FIXME what if a file was partially read in - this may return the wrong thing?
// FIXME need to make sure we extend the file to the maximum when creating it
func (fh *RWFileHandle) _size() int64 {
	if !fh.opened {
		return fh.file.Size()
	}
	fi, err := fh.fd.Stat()
	if err != nil {
		return 0
	}
	return fi.Size()
}

// Size returns the size of the underlying file
func (fh *RWFileHandle) Size() int64 {
	fh.mu.Lock()
	defer fh.mu.Unlock()
	return fh._size()
}

// Stat returns info about the file
func (fh *RWFileHandle) Stat() (os.FileInfo, error) {
	fh.mu.Lock()
	defer fh.mu.Unlock()
	return fh.file, nil
}

// _readAt bytes from the file at off
//
// call with lock held
func (fh *RWFileHandle) _readAt(b []byte, off int64) (n int, err error) {
	if fh.closed {
		return n, ECLOSED
	}
	if fh.flags&accessModeMask == os.O_WRONLY {
		return n, EBADF
	}
	if err = fh.openPending(false); err != nil {
		return n, err
	}
	return fh.fd.ReadAt(b, off)
}

// ReadAt bytes from the file at off
func (fh *RWFileHandle) ReadAt(b []byte, off int64) (n int, err error) {
	fh.mu.Lock()
	defer fh.mu.Unlock()
	return fh._readAt(b, off)
}

// Read bytes from the file
func (fh *RWFileHandle) Read(b []byte) (n int, err error) {
	fh.mu.Lock()
	defer fh.mu.Unlock()
	n, err = fh._readAt(b, fh.offset)
	fh.offset += int64(n)
	return n, err
}

// Seek to new file position
func (fh *RWFileHandle) Seek(offset int64, whence int) (ret int64, err error) {
	fh.mu.Lock()
	defer fh.mu.Unlock()
	if fh.closed {
		return 0, ECLOSED
	}
	if !fh.opened && offset == 0 && whence != 2 {
		return 0, nil
	}
	if err = fh.openPending(false); err != nil {
		return ret, err
	}
	switch whence {
	case io.SeekStart:
		fh.offset = 0
	case io.SeekEnd:
		fh.offset = fh._size()
	}
	fh.offset += offset
	// we don't check the offset - the next Read will
	return fh.offset, nil
}

// WriteAt bytes to the file at off
func (fh *RWFileHandle) _writeAt(b []byte, off int64) (n int, err error) {
	if fh.closed {
		return n, ECLOSED
	}
	if fh.flags&accessModeMask == os.O_RDONLY {
		return n, EBADF
	}
	if err = fh.openPending(false); err != nil {
		return n, err
	}
	fh.writeCalled = true

	if fh.flags&os.O_APPEND != 0 {
		// if append is set, call Write as WriteAt returns an error if append is set
		n, err = fh.fd.Write(b)
	} else {
		n, err = fh.fd.WriteAt(b, off)
	}
	if err != nil {
		return n, err
	}

	fi, err := fh.fd.Stat()
	if err != nil {
		return n, errors.Wrap(err, "failed to stat cache file")
	}
	fh.file.setSize(fi.Size())
	return n, err
}

// WriteAt bytes to the file at off
func (fh *RWFileHandle) WriteAt(b []byte, off int64) (n int, err error) {
	fh.mu.Lock()
	defer fh.mu.Unlock()
	return fh._writeAt(b, off)
}

// Write bytes to the file
func (fh *RWFileHandle) Write(b []byte) (n int, err error) {
	fh.mu.Lock()
	defer fh.mu.Unlock()
	n, err = fh._writeAt(b, fh.offset)
	fh.offset += int64(n)
	return n, err
}

// WriteString a string to the file
func (fh *RWFileHandle) WriteString(s string) (n int, err error) {
	return fh.Write([]byte(s))
}

// Truncate file to given size
func (fh *RWFileHandle) Truncate(size int64) (err error) {
	fh.mu.Lock()
	defer fh.mu.Unlock()
	if fh.closed {
		return ECLOSED
	}
	if err = fh.openPending(size == 0); err != nil {
		return err
	}
	fh.changed = true
	fh.file.setSize(size)
	return fh.fd.Truncate(size)
}

// Sync commits the current contents of the file to stable storage. Typically,
// this means flushing the file system's in-memory copy of recently written
// data to disk.
func (fh *RWFileHandle) Sync() error {
	fh.mu.Lock()
	defer fh.mu.Unlock()
	if fh.closed {
		return ECLOSED
	}
	if !fh.opened {
		return nil
	}
	if fh.flags&accessModeMask == os.O_RDONLY {
		return nil
	}
	return fh.fd.Sync()
}

func (fh *RWFileHandle) logPrefix() string {
	return fmt.Sprintf("%s(%p)", fh.file.Path(), fh)
}

// Chdir changes the current working directory to the file, which must
// be a directory.
func (fh *RWFileHandle) Chdir() error {
	return ENOSYS
}

// Chmod changes the mode of the file to mode.
func (fh *RWFileHandle) Chmod(mode os.FileMode) error {
	return ENOSYS
}

// Chown changes the numeric uid and gid of the named file.
func (fh *RWFileHandle) Chown(uid, gid int) error {
	return ENOSYS
}

// Fd returns the integer Unix file descriptor referencing the open file.
func (fh *RWFileHandle) Fd() uintptr {
	return fh.fd.Fd()
}

// Name returns the name of the file from the underlying Object.
func (fh *RWFileHandle) Name() string {
	return fh.file.String()
}

// Readdir reads the contents of the directory associated with file.
func (fh *RWFileHandle) Readdir(n int) ([]os.FileInfo, error) {
	return nil, ENOSYS
}

// Readdirnames reads the contents of the directory associated with file.
func (fh *RWFileHandle) Readdirnames(n int) (names []string, err error) {
	return nil, ENOSYS
}
