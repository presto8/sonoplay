//go:build windows

package main

// terminalSize is a no-op stub on Windows: this tool's window-size-aware
// truncation (see runList) is Linux/Unix-only for now, so just report
// "unknown" here and let callers fall back to printing everything.
func terminalSize(fd uintptr) (cols, rows int, ok bool) {
	return 0, 0, false
}
