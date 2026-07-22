//go:build linux

package app

// cowork_x11_linux.go is the native Linux GUI backend for cowork mode. It
// talks the X11 protocol directly via xgb (pure Go, no cgo), so computer use
// works out of the box on any Linux machine with an X server or XWayland —
// no xdotool/scrot/grim to install. cowork.go falls back to those tools when
// this backend can't reach an X server (e.g. a native Wayland session).

import (
	"bytes"
	"fmt"
	"image"
	"image/png"
	"sync"

	"github.com/jezek/xgb"
	"github.com/jezek/xgb/xproto"
	"github.com/jezek/xgb/xtest"
)

var (
	x11Once  sync.Once
	x11Conn_ *xgb.Conn
	x11Err   error
)

// x11Conn dials the X server once per process (honoring $DISPLAY and
// ~/.Xauthority via xgb) and initializes the XTEST extension.
func x11Conn() (*xgb.Conn, error) {
	x11Once.Do(func() {
		c, err := xgb.NewConn()
		if err != nil {
			x11Err = fmt.Errorf("cannot connect to X server: %w", err)
			return
		}
		if err := xtest.Init(c); err != nil {
			c.Close()
			x11Err = fmt.Errorf("XTEST extension: %w", err)
			return
		}
		x11Conn_ = c
	})
	if x11Conn_ == nil {
		return nil, x11Err
	}
	return x11Conn_, nil
}

// nativeBackendErr explains why the native backend is unavailable, for the
// fallback error messages in cowork.go.
func nativeBackendErr() error {
	_, err := x11Conn()
	return err
}

func x11Root(c *xgb.Conn) xproto.Window {
	return xproto.Setup(c).Roots[c.DefaultScreen].Root
}

// nativeScreen captures the root window via GetImage and reports the
// screen's pixel geometry.
func nativeScreen() (Image, screenGeom, error) {
	c, err := x11Conn()
	if err != nil {
		return Image{}, screenGeom{}, err
	}
	setup := xproto.Setup(c)
	scr := setup.Roots[c.DefaultScreen]
	geom := screenGeom{W: int(scr.WidthInPixels), H: int(scr.HeightInPixels)}
	rep, err := xproto.GetImage(c, xproto.ImageFormatZPixmap, xproto.Drawable(scr.Root),
		0, 0, scr.WidthInPixels, scr.HeightInPixels, 0xFFFFFFFF).Reply()
	if err != nil {
		return Image{}, geom, fmt.Errorf("GetImage: %w", err)
	}
	rgba, err := zpixmapToRGBA(setup, scr, rep)
	if err != nil {
		return Image{}, geom, err
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, rgba); err != nil {
		return Image{}, geom, err
	}
	return Image{MIME: "image/png", Data: buf.Bytes()}, geom, nil
}

// zpixmapToRGBA decodes a ZPixmap GetImage reply using the root visual's
// color masks and the connection's scanline padding.
func zpixmapToRGBA(setup *xproto.SetupInfo, scr xproto.ScreenInfo, rep *xproto.GetImageReply) (*image.RGBA, error) {
	var bitsPerPixel int
	for _, f := range setup.PixmapFormats {
		if f.Depth == rep.Depth {
			bitsPerPixel = int(f.BitsPerPixel)
			break
		}
	}
	if bitsPerPixel != 32 && bitsPerPixel != 24 && bitsPerPixel != 16 {
		return nil, fmt.Errorf("unsupported pixel format (depth %d, %d bpp)", rep.Depth, bitsPerPixel)
	}
	var rMask, gMask, bMask uint32
	found := false
	for _, d := range scr.AllowedDepths {
		if d.Depth != rep.Depth {
			continue
		}
		for _, v := range d.Visuals {
			if v.VisualId == scr.RootVisual {
				rMask, gMask, bMask = v.RedMask, v.GreenMask, v.BlueMask
				found = true
			}
		}
	}
	if !found {
		return nil, fmt.Errorf("root visual not found for depth %d", rep.Depth)
	}
	pad := int(setup.BitmapFormatScanlinePad)
	if pad == 0 {
		pad = 32
	}
	w, h := int(scr.WidthInPixels), int(scr.HeightInPixels)
	stride := zpixmapStride(w, bitsPerPixel, pad)
	bytesPerPixel := bitsPerPixel / 8
	littleEndian := setup.ImageByteOrder == 0 // LSBFirst
	rgba := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		row := y * stride
		if row+w*bytesPerPixel > len(rep.Data) {
			return nil, fmt.Errorf("GetImage data too short (%d bytes for %dx%d)", len(rep.Data), w, h)
		}
		for x := 0; x < w; x++ {
			o := row + x*bytesPerPixel
			var px uint32
			if littleEndian {
				for i := bytesPerPixel - 1; i >= 0; i-- {
					px = px<<8 | uint32(rep.Data[o+i])
				}
			} else {
				for i := 0; i < bytesPerPixel; i++ {
					px = px<<8 | uint32(rep.Data[o+i])
				}
			}
			d := rgba.PixOffset(x, y)
			rgba.Pix[d+0] = maskToByte(px, rMask)
			rgba.Pix[d+1] = maskToByte(px, gMask)
			rgba.Pix[d+2] = maskToByte(px, bMask)
			rgba.Pix[d+3] = 0xFF
		}
	}
	return rgba, nil
}

// fake sends one XTEST event and flushes the connection.
func fake(c *xgb.Conn, typ, detail byte, root xproto.Window, x, y int) error {
	xtest.FakeInput(c, typ, detail, xproto.TimeCurrentTime, root, int16(x), int16(y), 0)
	c.Sync() // round-trip: forces the request out and surfaces connection errors
	return nil
}

func nativeMove(x, y int) error {
	c, err := x11Conn()
	if err != nil {
		return err
	}
	return fake(c, xproto.MotionNotify, 0, x11Root(c), x, y)
}

// x11ButtonNumber maps our button names to X11 button numbers (1/2/3 =
// left/middle/right, 4/5 = wheel up/down, 6/7 = wheel left/right).
func x11ButtonNumber(button string) byte {
	switch button {
	case "right":
		return 3
	case "middle":
		return 2
	case "scrollup":
		return 4
	case "scrolldown":
		return 5
	case "scrollleft":
		return 6
	case "scrollright":
		return 7
	default:
		return 1
	}
}

func nativeClick(x, y int, button string, double bool) error {
	c, err := x11Conn()
	if err != nil {
		return err
	}
	root := x11Root(c)
	btn := x11ButtonNumber(button)
	if err := fake(c, xproto.MotionNotify, 0, root, x, y); err != nil {
		return err
	}
	clicks := 1
	if double {
		clicks = 2
	}
	for n := 0; n < clicks; n++ {
		if err := fake(c, xproto.ButtonPress, btn, root, x, y); err != nil {
			return err
		}
		if err := fake(c, xproto.ButtonRelease, btn, root, x, y); err != nil {
			return err
		}
	}
	return nil
}

func nativeScroll(dx, dy int) error {
	c, err := x11Conn()
	if err != nil {
		return err
	}
	root := x11Root(c)
	press := func(btn string, n int) error {
		b := x11ButtonNumber(btn)
		for i := 0; i < n; i++ {
			if err := fake(c, xproto.ButtonPress, b, root, 0, 0); err != nil {
				return err
			}
			if err := fake(c, xproto.ButtonRelease, b, root, 0, 0); err != nil {
				return err
			}
		}
		return nil
	}
	if dy > 0 {
		if err := press("scrolldown", dy); err != nil {
			return err
		}
	} else if dy < 0 {
		if err := press("scrollup", -dy); err != nil {
			return err
		}
	}
	if dx > 0 {
		return press("scrollright", dx)
	} else if dx < 0 {
		return press("scrollleft", -dx)
	}
	return nil
}

// x11Keycodes lazily caches keysym→keycode lookups for the process.
var x11KeycodeCache = map[uint32]xproto.Keycode{}

// x11Keycode finds a keycode producing ks, remapping the highest (usually
// unused) keycode when no key carries the keysym — the same trick xdotool
// uses for Unicode input.
func x11Keycode(c *xgb.Conn, ks uint32) (xproto.Keycode, error) {
	if kc, ok := x11KeycodeCache[ks]; ok {
		return kc, nil
	}
	setup := xproto.Setup(c)
	min, max := setup.MinKeycode, setup.MaxKeycode
	count := int(max) - int(min) + 1
	rep, err := xproto.GetKeyboardMapping(c, min, byte(count)).Reply()
	if err != nil {
		return 0, fmt.Errorf("GetKeyboardMapping: %w", err)
	}
	per := int(rep.KeysymsPerKeycode)
	for i := 0; i < count; i++ {
		for j := 0; j < per && i*per+j < len(rep.Keysyms); j++ {
			if uint32(rep.Keysyms[i*per+j]) == ks {
				kc := min + xproto.Keycode(i)
				x11KeycodeCache[ks] = kc
				return kc, nil
			}
		}
	}
	// No key carries the keysym: remap the top keycode to it.
	if per < 1 {
		per = 1
	}
	syms := make([]xproto.Keysym, per)
	syms[0] = xproto.Keysym(ks)
	if err := xproto.ChangeKeyboardMapping(c, 1, max, byte(per), syms).Check(); err != nil {
		return 0, fmt.Errorf("ChangeKeyboardMapping: %w", err)
	}
	x11KeycodeCache[ks] = max
	return max, nil
}

func x11TapKey(c *xgb.Conn, ks uint32) error {
	kc, err := x11Keycode(c, ks)
	if err != nil {
		return err
	}
	root := x11Root(c)
	if err := fake(c, xproto.KeyPress, byte(kc), root, 0, 0); err != nil {
		return err
	}
	return fake(c, xproto.KeyRelease, byte(kc), root, 0, 0)
}

func x11HoldKey(c *xgb.Conn, ks uint32, typ byte) error {
	kc, err := x11Keycode(c, ks)
	if err != nil {
		return err
	}
	return fake(c, typ, byte(kc), x11Root(c), 0, 0)
}

func nativeKeyPress(key string, mods []string) error {
	def, ok := lookupKey(key)
	if !ok {
		return fmt.Errorf("unknown key %q", key)
	}
	var ks uint32
	if def.x11Key != "" {
		ks, ok = x11Keysyms[def.x11Key]
		if !ok {
			return fmt.Errorf("no X11 keysym for %q", def.x11Key)
		}
	} else {
		ks = keysymForRune([]rune(key)[0])
	}
	c, err := x11Conn()
	if err != nil {
		return err
	}
	var held []uint32
	for _, m := range mods {
		if mks, ok := x11ModifierKeysyms[m]; ok {
			if err := x11HoldKey(c, mks, xproto.KeyPress); err != nil {
				return err
			}
			held = append(held, mks)
		}
	}
	err = x11TapKey(c, ks)
	for _, mks := range held {
		_ = x11HoldKey(c, mks, xproto.KeyRelease)
	}
	return err
}

func nativeType(text string) error {
	c, err := x11Conn()
	if err != nil {
		return err
	}
	for _, r := range text {
		if err := x11TapKey(c, keysymForRune(r)); err != nil {
			return err
		}
	}
	return nil
}
