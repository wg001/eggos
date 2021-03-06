package fs

import (
	"io"
	"math/rand"
	"os"
	"syscall"
	"unsafe"

	"github.com/icexin/eggos/console"
	"github.com/icexin/eggos/fs/mount"
	"github.com/icexin/eggos/kernel/isyscall"
	"github.com/icexin/eggos/sys"

	"github.com/spf13/afero"
)

var (
	inodes []*Inode

	Root = mount.NewMountableFs(afero.NewMemMapFs())
)

type Ioctler interface {
	Ioctl(op, arg uintptr) error
}

type Inode struct {
	File  io.ReadWriteCloser
	Fd    int
	inuse bool
}

func (i *Inode) Release() {
	i.inuse = false
	i.File = nil
	i.Fd = -1
}

func AllocInode() (int, *Inode) {
	var fd int
	var ni *Inode
	for i := range inodes {
		entry := inodes[i]
		if !entry.inuse {
			fd = i
			ni = entry
			break
		}
	}
	if fd == 0 {
		ni = new(Inode)
		fd = len(inodes)
		inodes = append(inodes, ni)
	}
	ni.inuse = true
	ni.Fd = fd
	return fd, ni
}

func AllocFileNode(r io.ReadWriteCloser) (int, *Inode) {
	fd, ni := AllocInode()
	ni.File = r
	return fd, ni
}

func GetInode(fd int) (*Inode, error) {
	if int(fd) >= len(inodes) {
		return nil, syscall.EBADF
	}
	ni := inodes[fd]
	if !ni.inuse {
		return nil, syscall.EBADF
	}
	return ni, nil
}

func fscall(fn int) isyscall.Handler {
	return func(c *isyscall.Request) {
		var err error
		if fn == syscall.SYS_OPENAT {
			var fd int
			fd, err = sysOpen(c.Args[0], c.Args[1], c.Args[2], c.Args[3])
			if err != nil {
				c.Ret = isyscall.Error(err)
			} else {
				c.Ret = uintptr(fd)
			}
			c.Done()
			return
		}

		var ni *Inode

		ni, err = GetInode(int(c.Args[0]))
		if err != nil {
			c.Ret = isyscall.Error(err)
			c.Done()
			return
		}

		switch fn {
		case syscall.SYS_READ:
			var n int
			n, err = sysRead(ni, c.Args[1], c.Args[2])
			c.Ret = uintptr(n)
		case syscall.SYS_WRITE:
			var n int
			n, err = sysWrite(ni, c.Args[1], c.Args[2])
			c.Ret = uintptr(n)
		case syscall.SYS_CLOSE:
			err = sysClose(ni)
		case syscall.SYS_FSTAT64:
			err = sysStat(ni, c.Args[1])
		case syscall.SYS_IOCTL:
			err = sysIoctl(ni, c.Args[1], c.Args[2])
		}

		if err != nil {
			c.Ret = isyscall.Error(err)
		}
		c.Done()
	}
}

func sysOpen(dirfd, name, flags, perm uintptr) (int, error) {
	path := cstring(name)
	fd, ni := AllocInode()
	f, err := Root.OpenFile(path, int(flags), os.FileMode(perm))
	if err != nil {
		if os.IsNotExist(err) {
			return 0, syscall.ENOENT
		}
		return 0, err
	}
	ni.File = f
	return fd, nil
}

func sysClose(ni *Inode) error {
	err := ni.File.Close()
	ni.Release()
	return err
}

func sysRead(ni *Inode, p, n uintptr) (int, error) {
	buf := sys.UnsafeBuffer(p, int(n))
	ret, err := ni.File.Read(buf)

	switch {
	case ret != 0:
		return ret, nil
	case err == io.EOF:
		return 0, nil
	case err != nil:
		return 0, err
	default:
		return ret, err
	}
}

func sysWrite(ni *Inode, p, n uintptr) (int, error) {
	buf := sys.UnsafeBuffer(p, int(n))
	_n, err := ni.File.Write(buf)
	if _n != 0 {
		return _n, nil
	}
	return 0, err
}

func sysStat(ni *Inode, statptr uintptr) error {
	file, ok := ni.File.(afero.File)
	if !ok {
		return syscall.EINVAL
	}
	stat := (*syscall.Stat_t)(unsafe.Pointer(statptr))
	info, err := file.Stat()
	if err != nil {
		return err
	}
	stat.Mode = uint32(info.Mode())
	stat.Mtim.Sec = int32(info.ModTime().Unix())
	stat.Size = info.Size()

	return nil
}

func sysIoctl(ni *Inode, op, arg uintptr) error {
	ctl, ok := ni.File.(Ioctler)
	if !ok {
		return syscall.EINVAL
	}
	return ctl.Ioctl(op, arg)
}

func sysFcntl(call *isyscall.Request) {
	call.Ret = 0
	call.Done()
}

// func Uname(buf *Utsname)
func sysUname(c *isyscall.Request) {
	unsafebuf := func(b *[65]int8) []byte {
		return (*[65]byte)(unsafe.Pointer(b))[:]
	}
	buf := (*syscall.Utsname)(unsafe.Pointer(c.Args[0]))
	copy(unsafebuf(&buf.Machine), "x86_32")
	copy(unsafebuf(&buf.Domainname), "icexin.com")
	copy(unsafebuf(&buf.Nodename), "icexin.local")
	copy(unsafebuf(&buf.Release), "0")
	copy(unsafebuf(&buf.Sysname), "eggos")
	copy(unsafebuf(&buf.Version), "0")
	c.Ret = 0
	c.Done()
}

// func fstatat(dirfd int, path string, stat *Stat_t, flags int)
func sysFstatat64(c *isyscall.Request) {
	name := cstring(c.Args[1])
	stat := (*syscall.Stat_t)(unsafe.Pointer(c.Args[2]))
	info, err := Root.Stat(name)
	if err != nil {
		if os.IsNotExist(err) {
			c.Ret = isyscall.Errno(syscall.ENOENT)
		} else {
			c.Ret = isyscall.Error(err)
		}
		c.Done()
		return
	}
	stat.Mode = uint32(info.Mode())
	stat.Mtim.Sec = int32(info.ModTime().Unix())
	stat.Size = info.Size()
	c.Ret = 0
	c.Done()
}

func sysRandom(call *isyscall.Request) {
	p, n := call.Args[0], call.Args[1]
	buf := sys.UnsafeBuffer(p, int(n))
	rand.Read(buf)
	call.Ret = n
	call.Done()
}

func cstring(ptr uintptr) string {
	var n int
	for p := ptr; *(*byte)(unsafe.Pointer(p)) != 0; p++ {
		n++
	}
	return string(sys.UnsafeBuffer(ptr, n))
}

type fileHelper struct {
	r io.Reader
	w io.Writer
	c io.Closer
}

func NewFile(r io.Reader, w io.Writer, c io.Closer) io.ReadWriteCloser {
	return &fileHelper{
		r: r,
		w: w,
		c: c,
	}
}

func (r *fileHelper) Read(p []byte) (int, error) {
	if r.r != nil {
		return r.r.Read(p)
	}
	return 0, syscall.EINVAL
}
func (r *fileHelper) Write(p []byte) (int, error) {
	if r.w != nil {
		return r.w.Write(p)
	}
	return 0, syscall.EROFS
}

func (r *fileHelper) Ioctl(op, arg uintptr) error {
	var x interface{}
	if r.r != nil {
		x = r.r
	} else {
		x = r.w
	}
	ctl, ok := x.(Ioctler)
	if !ok {
		return syscall.EBADF
	}
	return ctl.Ioctl(op, arg)
}

func (r *fileHelper) Close() error {
	if r.c != nil {
		return r.c.Close()
	}
	return syscall.EINVAL
}

func Mount(target string, fs afero.Fs) error {
	return Root.Mount(target, fs)
}

func vfsInit() {
	c := console.Console()
	// stdin
	AllocFileNode(NewFile(c, nil, nil))
	// stdout
	AllocFileNode(NewFile(nil, c, nil))
	// stderr
	AllocFileNode(NewFile(nil, c, nil))
	// epoll fd
	AllocFileNode(NewFile(nil, nil, nil))

	etcInit()
}

func sysInit() {
	isyscall.Register(syscall.SYS_OPENAT, fscall(syscall.SYS_OPENAT))
	isyscall.Register(syscall.SYS_WRITE, fscall(syscall.SYS_WRITE))
	isyscall.Register(syscall.SYS_READ, fscall(syscall.SYS_READ))
	isyscall.Register(syscall.SYS_CLOSE, fscall(syscall.SYS_CLOSE))
	isyscall.Register(syscall.SYS_FSTAT64, fscall(syscall.SYS_FSTAT64))
	isyscall.Register(syscall.SYS_IOCTL, fscall(syscall.SYS_IOCTL))
	isyscall.Register(syscall.SYS_FCNTL, sysFcntl)
	isyscall.Register(syscall.SYS_FCNTL64, sysFcntl)
	isyscall.Register(syscall.SYS_FSTATAT64, sysFstatat64)
	isyscall.Register(syscall.SYS_UNAME, sysUname)
	isyscall.Register(355, sysRandom)
}

func Init() {
	vfsInit()
	sysInit()
}
