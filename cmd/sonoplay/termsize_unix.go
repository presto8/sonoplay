//go:build !windows

package main

import (
	"syscall"
	"unsafe"
)

// unixWinsize mirrors the kernel's struct winsize (see ioctl_tty(2)).
type unixWinsize struct {
	Row, Col, Xpixel, Ypixel uint16
}

// terminalSize reports the width and height of the terminal attached to fd
// (e.g. os.Stdin.Fd()). ok is false if fd isn't a terminal, or its size
// can't be determined for any other reason (no controlling terminal,
// output redirected to a file/pipe, etc.) - callers should treat that as
// "unknown, don't truncate".
func terminalSize(fd uintptr) (cols, rows int, ok bool) {
	var ws unixWinsize
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd, uintptr(syscall.TIOCGWINSZ), uintptr(unsafe.Pointer(&ws)))
	if errno != 0 || (ws.Row == 0 && ws.Col == 0) {
		return 0, 0, false
	}
	return int(ws.Col), int(ws.Row), true
}
