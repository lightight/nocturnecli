package app

import "testing"

func TestKeysymForRune(t *testing.T) {
	cases := []struct {
		r    rune
		want uint32
	}{
		{'a', 0x61},
		{'A', 0x41},
		{' ', 0x20},
		{'~', 0x7E},
		{'\n', 0xFF0D}, // Return
		{'\t', 0xFF09}, // Tab
		{'é', 0xE9},    // Latin-1 is its own keysym
		{'€', 0x010020AC},
		{'世', 0x01004E16},
	}
	for _, c := range cases {
		if got := keysymForRune(c.r); got != c.want {
			t.Errorf("keysymForRune(%q) = %#x, want %#x", c.r, got, c.want)
		}
	}
}

func TestAllNamedKeysHaveX11Keysyms(t *testing.T) {
	// Every named key in the shared keyNames table must resolve to a numeric
	// keysym, or native key_press would fail for it on Linux.
	for name, def := range keyNames {
		if def.x11Key == "" {
			t.Errorf("key %q has no x11Key spelling", name)
			continue
		}
		if _, ok := x11Keysyms[def.x11Key]; !ok {
			t.Errorf("key %q: x11Key %q missing from x11Keysyms", name, def.x11Key)
		}
	}
}

func TestX11ModifierKeysyms(t *testing.T) {
	for _, m := range []string{"ctrl", "control", "alt", "option", "shift", "cmd", "command", "super", "win"} {
		if _, ok := x11ModifierKeysyms[m]; !ok {
			t.Errorf("modifier %q missing from x11ModifierKeysyms", m)
		}
	}
}

func TestZpixmapStride(t *testing.T) {
	cases := []struct {
		w, bpp, pad, want int
	}{
		{5, 32, 32, 20}, // 160 bits, already pad-aligned
		{3, 24, 32, 12}, // 72 bits -> 96 bits = 12 bytes
		{1920, 32, 32, 7680},
		{1, 24, 32, 4}, // 24 bits -> 32 bits = 4 bytes
	}
	for _, c := range cases {
		if got := zpixmapStride(c.w, c.bpp, c.pad); got != c.want {
			t.Errorf("zpixmapStride(%d, %d, %d) = %d, want %d", c.w, c.bpp, c.pad, got, c.want)
		}
	}
}

func TestMaskToByte(t *testing.T) {
	// Typical 32-bit XRGB layout.
	pixel := uint32(0x00C86432)
	if got := maskToByte(pixel, 0xFF0000); got != 0xC8 {
		t.Errorf("red = %#x, want 0xc8", got)
	}
	if got := maskToByte(pixel, 0x00FF00); got != 0x64 {
		t.Errorf("green = %#x, want 0x64", got)
	}
	if got := maskToByte(pixel, 0x0000FF); got != 0x32 {
		t.Errorf("blue = %#x, want 0x32", got)
	}
	// 16-bit RGB565: full-scale channel must scale to 0xFF, not truncate.
	if got := maskToByte(0xF800, 0xF800); got != 0xFF {
		t.Errorf("rgb565 red = %#x, want 0xff", got)
	}
	if got := maskToByte(0, 0xF800); got != 0 {
		t.Errorf("zero pixel red = %#x, want 0", got)
	}
	if got := maskToByte(0xFFFF, 0); got != 0 {
		t.Errorf("zero mask = %#x, want 0", got)
	}
}
