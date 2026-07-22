package app

// cowork_x11.go holds the platform-independent helpers behind the native
// Linux X11 backend (cowork_x11_linux.go). Everything here is pure data and
// arithmetic — no xgb imports — so it compiles and is unit-tested on every
// platform.

// x11Keysyms maps the xdotool keysym names used by the keyNames table onto
// numeric X11 keysyms for the native backend.
var x11Keysyms = map[string]uint32{
	"Return":    0xFF0D,
	"Tab":       0xFF09,
	"Escape":    0xFF1B,
	"space":     0x0020,
	"BackSpace": 0xFF08,
	"Delete":    0xFFFF,
	"Up":        0xFF52,
	"Down":      0xFF54,
	"Left":      0xFF51,
	"Right":     0xFF53,
	"Home":      0xFF50,
	"End":       0xFF57,
	"Page_Up":   0xFF55,
	"Page_Down": 0xFF56,
	"F1":        0xFFBE,
	"F2":        0xFFBF,
	"F3":        0xFFC0,
	"F4":        0xFFC1,
	"F5":        0xFFC2,
	"F6":        0xFFC3,
	"F7":        0xFFC4,
	"F8":        0xFFC5,
	"F9":        0xFFC6,
	"F10":       0xFFC7,
	"F11":       0xFFC8,
	"F12":       0xFFC9,
}

// x11ModifierKeysyms resolves modifier names (same spellings as
// osKeyPress accepts) to their left-hand keysyms.
var x11ModifierKeysyms = map[string]uint32{
	"shift":   0xFFE1, // Shift_L
	"ctrl":    0xFFE3, // Control_L
	"control": 0xFFE3,
	"alt":     0xFFE9, // Alt_L
	"option":  0xFFE9,
	"cmd":     0xFFEB, // Super_L
	"command": 0xFFEB,
	"super":   0xFFEB,
	"win":     0xFFEB,
}

// keysymForRune converts a character to an X11 keysym. Printable ASCII and
// Latin-1 are their own keysyms; everything else uses the Unicode keysym
// convention 0x01000000 | codepoint.
func keysymForRune(r rune) uint32 {
	switch {
	case r == '\n':
		return 0xFF0D // Return
	case r == '\t':
		return 0xFF09 // Tab
	case r >= 0x20 && r <= 0xFF:
		return uint32(r)
	default:
		return 0x01000000 | uint32(r)
	}
}

// zpixmapStride computes the bytes per scanline in a ZPixmap GetImage reply:
// the pixel data padded up to the connection's scanline pad (in bits).
func zpixmapStride(width, bitsPerPixel, scanlinePadBits int) int {
	bits := width * bitsPerPixel
	return (bits + scanlinePadBits - 1) / scanlinePadBits * (scanlinePadBits / 8)
}

// maskToByte extracts the component selected by mask from a pixel value and
// scales it to 8 bits.
func maskToByte(pixel, mask uint32) uint8 {
	if mask == 0 {
		return 0
	}
	shift := 0
	for m := mask; m&1 == 0; m >>= 1 {
		shift++
	}
	bits := 0
	for m := mask >> shift; m&1 == 1; m >>= 1 {
		bits++
	}
	v := (pixel & mask) >> shift
	max := uint32(1)<<bits - 1
	return uint8(v * 255 / max)
}
