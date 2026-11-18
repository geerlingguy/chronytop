//go:build linux || darwin

package main

import (
	"syscall"
	"unsafe"
)

func ioctl(fd, req uintptr, arg unsafe.Pointer) error {
	// #nosec G103 -- ioctl needs a pointer to the termios/winsize struct.
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, fd, req, uintptr(arg)); e != 0 {
		return e
	}
	return nil
}

// makeRaw puts the terminal in raw mode and returns the previous state.
func makeRaw(fd uintptr) (*syscall.Termios, error) {
	var old syscall.Termios
	if err := ioctl(fd, ioctlGetTermios, unsafe.Pointer(&old)); err != nil { // #nosec G103 -- ioctl needs a struct pointer.
		return nil, err
	}
	t := old
	t.Iflag &^= syscall.IGNBRK | syscall.BRKINT | syscall.PARMRK | syscall.ISTRIP |
		syscall.INLCR | syscall.IGNCR | syscall.ICRNL | syscall.IXON
	t.Oflag &^= syscall.OPOST
	t.Lflag &^= syscall.ECHO | syscall.ECHONL | syscall.ICANON | syscall.ISIG | syscall.IEXTEN
	t.Cflag &^= syscall.CSIZE | syscall.PARENB
	t.Cflag |= syscall.CS8
	t.Cc[syscall.VMIN] = 1
	t.Cc[syscall.VTIME] = 0
	if err := ioctl(fd, ioctlSetTermios, unsafe.Pointer(&t)); err != nil { // #nosec G103 -- ioctl needs a struct pointer.
		return nil, err
	}
	return &old, nil
}

func restore(fd uintptr, t *syscall.Termios) {
	_ = ioctl(fd, ioctlSetTermios, unsafe.Pointer(t)) // #nosec G103 -- ioctl needs a struct pointer.
}

func termSize(fd uintptr) (w, h int) {
	var ws struct{ Row, Col, X, Y uint16 }
	if err := ioctl(fd, syscall.TIOCGWINSZ, unsafe.Pointer(&ws)); err != nil || ws.Col == 0 { // #nosec G103 -- ioctl needs a struct pointer.
		return 80, 24
	}
	return int(ws.Col), int(ws.Row)
}
