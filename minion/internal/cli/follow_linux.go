package cli

import (
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

const sysPidfdOpen = 434 // the same number on every Linux architecture

// follow copies the job's log to w as it grows and returns the job's exit code
// once its wrapper exits. It sleeps in epoll on an inotify watch of the log and
// a pidfd of the wrapper: no timer, no polling.
func follow(dir string, w io.Writer) (int, error) {
	log, err := os.Open(filepath.Join(dir, "log"))
	if err != nil {
		return 1, err
	}
	defer log.Close()
	ep, err := syscall.EpollCreate1(syscall.EPOLL_CLOEXEC)
	if err != nil {
		return 1, err
	}
	defer syscall.Close(ep)
	in, err := syscall.InotifyInit1(syscall.IN_CLOEXEC)
	if err != nil {
		return 1, err
	}
	defer syscall.Close(in)
	if _, err := syscall.InotifyAddWatch(in, log.Name(), syscall.IN_MODIFY); err != nil {
		return 1, err
	}
	pidfd := wrapperPidfd(dir)
	if pidfd >= 0 {
		defer syscall.Close(pidfd)
	}
	for _, fd := range []int{in, pidfd} {
		if fd >= 0 {
			ev := syscall.EpollEvent{Events: syscall.EPOLLIN, Fd: int32(fd)}
			if err := syscall.EpollCtl(ep, syscall.EPOLL_CTL_ADD, fd, &ev); err != nil {
				return 1, err
			}
		}
	}
	events := make([]syscall.EpollEvent, 2)
	buf := make([]byte, 4096)
	for done := pidfd < 0; ; {
		if _, err := io.Copy(w, log); err != nil {
			return 1, err
		}
		if done {
			break
		}
		n, err := syscall.EpollWait(ep, events, -1)
		if err == syscall.EINTR {
			continue
		}
		if err != nil {
			return 1, err
		}
		for _, ev := range events[:n] {
			if int(ev.Fd) == pidfd {
				done = true
			} else {
				syscall.Read(in, buf) // drain; the copy above reads the new bytes
			}
		}
	}
	return result(dir)
}

// wrapperPidfd opens a pidfd on the job's running wrapper, or returns -1 if the
// job has finished or runs on another host (its pid means nothing here).
func wrapperPidfd(dir string) int {
	if _, ok := jobStatus(dir); ok || !onThisHost(dir) {
		return -1
	}
	b, err := os.ReadFile(filepath.Join(dir, "pid"))
	if err != nil {
		return -1
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		return -1
	}
	fd, _, errno := syscall.Syscall(sysPidfdOpen, uintptr(pid), 0, 0)
	if errno != 0 {
		return -1
	}
	return int(fd)
}
