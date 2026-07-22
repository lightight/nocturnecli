//go:build !linux

package app

// cowork_x11_other.go stubs out the native X11 backend on non-Linux builds;
// macOS and Windows use their own OS backends and never call these.

import "errors"

var errNoNativeX11 = errors.New("native X11 backend is only available on Linux")

func nativeScreen() (Image, screenGeom, error) { return Image{}, screenGeom{}, errNoNativeX11 }
func nativeMove(x, y int) error                { return errNoNativeX11 }
func nativeClick(x, y int, button string, double bool) error {
	return errNoNativeX11
}
func nativeScroll(dx, dy int) error { return errNoNativeX11 }
func nativeType(text string) error  { return errNoNativeX11 }
func nativeKeyPress(key string, mods []string) error {
	return errNoNativeX11
}
func nativeBackendErr() error { return errNoNativeX11 }
