//go:build windows

package app

import "golang.org/x/sys/windows"

// kernel32's SetConsoleCP / SetConsoleOutputCP aren't wrapped by x/sys, so we
// bind them lazily ourselves. Using the DLL that x/sys already links keeps this
// dependency-free.
var (
	kernel32               = windows.NewLazySystemDLL("kernel32.dll")
	procSetConsoleCP       = kernel32.NewProc("SetConsoleCP")
	procSetConsoleOutputCP = kernel32.NewProc("SetConsoleOutputCP")
)

// cpUTF8 is the Windows UTF-8 code page (same as `chcp 65001`).
const cpUTF8 = 65001

// EnableConsoleUTF8 switches the Windows console to the UTF-8 code page for both
// output and input, so the box-drawing characters, arrows and emoji the TUI
// emits render correctly instead of as mojibake. Legacy consoles default to an
// OEM code page (437/850/1252) that mangles multi-byte UTF-8. It is best-effort:
// errors (e.g. output redirected to a file or pipe) are ignored.
func EnableConsoleUTF8() {
	procSetConsoleOutputCP.Call(uintptr(cpUTF8))
	procSetConsoleCP.Call(uintptr(cpUTF8))
}
