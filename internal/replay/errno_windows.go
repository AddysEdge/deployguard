//go:build windows

package replay

import "syscall"

// Winsock reports WSAECONNREFUSED (10061) and WSAECONNRESET (10054).
var (
	refusedErrnos = []syscall.Errno{syscall.ECONNREFUSED, 10061}
	resetErrnos   = []syscall.Errno{syscall.ECONNRESET, 10054, 10053}
)
