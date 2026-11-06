package tools

import (
	"encoding/binary"
	"syscall"
)

// codeFilter is the seccomp program for run_code, in the form bwrap reads. It's an allowlist,
// so anything new in the kernel starts out refused, which covers what's most worth keeping
// away from code: io_uring, bpf, perf_event_open, userfaultfd, keyctl, ptrace, mount, unshare
// and sockets that aren't local. None of those are on the list. Numbers are x86-64's, and any
// other calling convention, which a process can reach on purpose, ends the process.
func codeFilter() ([]byte, error) {
	const (
		ld   = 0x20 // load 32 bits at an offset of the syscall's data
		jeq  = 0x15
		jset = 0x45
		ret  = 0x06

		allow = 0x7fff0000
		kill  = 0x80000000
		errno = 0x00050000

		archX8664 = 0xc000003e
		x32Bit    = 0x40000000
		offNr     = 0
		offArch   = 4
		offArg0   = 16
	)
	refuse := uint32(errno | uint32(syscall.EPERM))
	type ins struct {
		code   uint16
		jt, jf uint8
		k      uint32
	}
	load := func(off uint32) ins { return ins{code: ld, k: off} }
	giveBack := func(k uint32) ins { return ins{code: ret, k: k} }

	prog := []ins{
		load(offArch),
		{code: jeq, jt: 1, k: archX8664},
		giveBack(kill),
		load(offNr),
		{code: jset, jf: 1, k: x32Bit},
		giveBack(refuse),
	}
	// A syscall with limits on its first argument: only these values, or none of these bits.
	only := func(nr uint32, values ...uint32) {
		block := []ins{load(offArg0)}
		for _, v := range values {
			block = append(block, ins{code: jeq, jf: 1, k: v}, giveBack(allow))
		}
		block = append(block, giveBack(refuse))
		prog = append(prog, ins{code: jeq, jf: uint8(len(block)), k: nr})
		prog = append(prog, block...)
	}
	const (
		sysClone        = 56
		sysClone3       = 435
		cloneNamespaces = 0x80 | 0x20000 | 0x02000000 | 0x04000000 | 0x08000000 | 0x10000000 | 0x20000000 | 0x40000000
		afUnix          = 1
	)
	only(41, afUnix)                 // socket: nothing but local sockets
	only(53, afUnix)                 // socketpair
	only(157, 15, 16, 3, 0x53564d41) // prctl: name a thread or a mapping, or ask if it's dumpable
	// ioctl, on its request, which is the second argument: enough to ask about terminals and
	// file descriptors, and not enough to push input into one.
	{
		requests := []uint32{0x5401 /* TCGETS */, 0x5413 /* TIOCGWINSZ */, 0x541B /* FIONREAD */, 0x5421 /* FIONBIO */, 0x5451 /* FIOCLEX */, 0x5450 /* FIONCLEX */}
		block := []ins{load(offArg0 + 8)}
		for _, v := range requests {
			block = append(block, ins{code: jeq, jf: 1, k: v}, giveBack(allow))
		}
		block = append(block, giveBack(errno|uint32(syscall.ENOTTY)))
		prog = append(prog, ins{code: jeq, jf: uint8(len(block)), k: 16})
		prog = append(prog, block...)
	}
	// clone: threads and processes, but no new namespaces.
	prog = append(prog,
		ins{code: jeq, jf: 4, k: sysClone},
		load(offArg0),
		ins{code: jset, jf: 1, k: cloneNamespaces},
		giveBack(refuse),
		giveBack(allow),
	)
	// clone3 keeps its flags where a filter can't read them. Saying it doesn't exist makes
	// libc fall back to clone.
	prog = append(prog, ins{code: jeq, jf: 1, k: sysClone3}, giveBack(errno|uint32(syscall.ENOSYS)))
	// The checks above load other values, but every path through them returns, so the
	// syscall number is still what's loaded for anything that gets here.
	for _, nr := range codeSyscalls {
		prog = append(prog, ins{code: jeq, jf: 1, k: nr}, giveBack(allow))
	}
	prog = append(prog, giveBack(refuse))

	out := make([]byte, 0, len(prog)*8)
	for _, in := range prog {
		out = binary.NativeEndian.AppendUint16(out, in.code)
		out = append(out, in.jt, in.jf)
		out = binary.NativeEndian.AppendUint32(out, in.k)
	}
	return out, nil
}

// codeSyscalls is what ordinary Python needs: files, memory, threads, processes, signals,
// time and local sockets.
var codeSyscalls = []uint32{
	0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, // read write open close stat fstat lstat poll lseek mmap mprotect munmap brk
	13, 14, 15, 17, 18, 19, 20, 21, 22, 23, 24, 25, // rt_sigaction rt_sigprocmask rt_sigreturn pread64 pwrite64 readv writev access pipe select sched_yield mremap
	26, 27, 28, 32, 33, 34, 35, 36, 37, 38, 39, 40, // msync mincore madvise dup dup2 pause nanosleep getitimer alarm setitimer getpid sendfile
	42, 43, 44, 45, 46, 47, 48, 49, 50, 51, 52, 54, 55, // connect accept sendto recvfrom sendmsg recvmsg shutdown bind listen getsockname getpeername setsockopt getsockopt
	57, 58, 59, 60, 61, 62, 63, // fork vfork execve exit wait4 kill uname
	72, 73, 74, 75, 76, 77, 78, 79, 80, 81, 82, 83, 84, 85, 86, 87, 88, 89, 90, 91, // fcntl flock fsync fdatasync truncate ftruncate getdents getcwd chdir fchdir rename mkdir rmdir creat link unlink symlink readlink chmod fchmod
	95, 96, 97, 98, 99, 100, // umask gettimeofday getrlimit getrusage sysinfo times
	102, 104, 107, 108, 109, 110, 111, 112, 118, 120, 121, 124, // getuid getgid geteuid getegid setpgid getppid getpgrp setsid getresuid getresgid getpgid getsid
	131, 132, 137, 138, 140, // sigaltstack utime statfs fstatfs getpriority
	143, 145, 146, 147, 158, // sched_getparam sched_getscheduler sched_get_priority_max sched_get_priority_min arch_prctl
	186, 191, 192, 193, 194, 195, 196, 200, 201, 202, // gettid getxattr lgetxattr fgetxattr listxattr llistxattr flistxattr tkill time futex
	204, 217, 218, 219, 228, 229, 230, 231, 232, 233, 234, 235, // sched_getaffinity getdents64 set_tid_address restart_syscall clock_gettime clock_getres clock_nanosleep exit_group epoll_wait epoll_ctl tgkill utimes
	257, 258, 261, 262, 263, 264, 265, 266, 267, 268, 269, // openat mkdirat futimesat newfstatat unlinkat renameat linkat symlinkat readlinkat fchmodat faccessat
	270, 271, 273, 274, 280, 281, 282, 283, 284, 285, 286, 287, 288, 289, 290, 291, 292, 293, // pselect6 ppoll set_robust_list get_robust_list utimensat epoll_pwait signalfd timerfd_create eventfd fallocate timerfd_settime timerfd_gettime accept4 signalfd4 eventfd2 epoll_create1 dup3 pipe2
	213, 302, 316, 318, 324, 326, 332, 334, 439, 436, 441, // epoll_create prlimit64 renameat2 getrandom membarrier copy_file_range statx rseq faccessat2 close_range epoll_pwait2
}
