//go:build !windows

package replay

import "syscall"

var (
	refusedErrnos = []syscall.Errno{syscall.ECONNREFUSED}
	resetErrnos   = []syscall.Errno{syscall.ECONNRESET, syscall.ECONNABORTED, syscall.EPIPE}
)
