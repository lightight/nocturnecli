//go:build !windows

package app

// EnableConsoleUTF8 is a no-op on non-Windows platforms, where terminals are
// already UTF-8.
func EnableConsoleUTF8() {}
