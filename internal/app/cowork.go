package app

import (
	"bytes"
	"encoding/json"
	"fmt"
	"image"
	"image/jpeg"
	"image/png"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

// cowork.go implements the computer-use tools available in cowork mode:
// screenshot, click, move_mouse, scroll, type_text, key_press, and open_app.
// macOS and Windows shell out to built-in platform facilities (osascript/JXA,
// PowerShell); Linux prefers the native pure-Go X11 backend in
// cowork_x11_linux.go and falls back to xdotool & friends. GUI control on
// macOS requires the terminal to hold the Accessibility permission
// (System Settings → Privacy & Security).

// accessibilityHint is appended to GUI-action failures so a permission
// problem is diagnosable from the tool result alone.
const accessibilityHint = " — if this is a permissions error, grant your terminal the Accessibility permission in System Settings → Privacy & Security → Accessibility"

// --- tool entry points -----------------------------------------------------

// screenshotTool captures the screen. The image travels back to the model
// alongside the textual result (see executeWithImage). The result text reports
// both the screen's coordinate space and the delivered image size so the model
// can convert pixel positions into click coordinates.
func screenshotTool() (string, *Image) {
	img, geom, err := captureScreen()
	if err != nil {
		return "Error: screenshot failed: " + err.Error(), nil
	}
	img = shrinkScreenshot(img)
	info := "Click/move/scroll coordinates use the screen's own units."
	if geom.W > 0 && img.W > 0 {
		info = fmt.Sprintf("The screen is %d×%d in the units click/move/scroll take. The attached image is %d×%d px, so multiply a pixel position in the image by %.3f to get click coordinates.",
			geom.W, geom.H, img.W, img.H, float64(geom.W)/float64(img.W))
	}
	return "Screenshot captured. " + info, &img
}

func clickTool(a map[string]any) string {
	x, y := argInt(a, "x"), argInt(a, "y")
	button := strings.ToLower(strings.TrimSpace(argStr(a, "button")))
	if button == "" {
		button = "left"
	}
	if button != "left" && button != "right" && button != "middle" {
		return "Error: click 'button' must be left, right, or middle"
	}
	label := button + " click"
	if argBool(a, "double") {
		label = "double " + label
	}
	if err := osClick(x, y, button, argBool(a, "double")); err != nil {
		return "Error: click failed: " + err.Error()
	}
	return fmt.Sprintf("Clicked (%s) at (%d, %d). Take a screenshot to see the result.", label, x, y)
}

func moveMouseTool(a map[string]any) string {
	x, y := argInt(a, "x"), argInt(a, "y")
	if err := osMoveMouse(x, y); err != nil {
		return "Error: move_mouse failed: " + err.Error()
	}
	return fmt.Sprintf("Mouse moved to (%d, %d).", x, y)
}

func scrollTool(a map[string]any) string {
	dx, dy := argInt(a, "dx"), argInt(a, "dy")
	if dx == 0 && dy == 0 {
		return "Error: scroll requires a non-zero 'dy' (and/or 'dx')"
	}
	if x, y := argInt(a, "x"), argInt(a, "y"); x != 0 || y != 0 {
		_ = osMoveMouse(x, y) // best effort: position the pointer over the target first
	}
	if err := osScroll(dx, dy); err != nil {
		return "Error: scroll failed: " + err.Error()
	}
	dir := "down"
	if dy < 0 {
		dir = "up"
	}
	return fmt.Sprintf("Scrolled %s %d lines. Take a screenshot to see the result.", dir, abs(dy))
}

func typeTextTool(a map[string]any) string {
	text := argStr(a, "text")
	if text == "" {
		return "Error: type_text requires 'text'"
	}
	if err := osTypeText(text); err != nil {
		return "Error: type_text failed: " + err.Error()
	}
	return fmt.Sprintf("Typed %d characters into the focused window.", len([]rune(text)))
}

func keyPressTool(a map[string]any) string {
	key := strings.ToLower(strings.TrimSpace(argStr(a, "key")))
	if key == "" {
		return "Error: key_press requires 'key' (e.g. \"enter\", \"tab\", \"a\")"
	}
	mods := toStrings(a["modifiers"])
	if err := osKeyPress(key, mods); err != nil {
		return "Error: key_press failed: " + err.Error()
	}
	combo := key
	if len(mods) > 0 {
		combo = strings.Join(mods, "+") + "+" + key
	}
	return "Pressed " + combo + "."
}

func openAppTool(workdir string, a map[string]any) string {
	target := strings.TrimSpace(argStr(a, "target"))
	if target == "" {
		return "Error: open_app requires 'target' (an app name, file path, folder, or URL)"
	}
	if !strings.Contains(target, "://") && !filepath.IsAbs(target) && strings.ContainsAny(target, `/\`) {
		target = resolve(workdir, target)
	}
	if err := osOpen(target); err != nil {
		return "Error: open_app failed: " + err.Error()
	}
	return "Opened " + target + "."
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

// --- screen capture --------------------------------------------------------

// screenGeom is the screen's size in the units click/move/scroll take
// (points on macOS, pixels on Windows/Linux). Zero means unknown.
type screenGeom struct{ W, H int }

// captureScreen grabs the whole screen and reports the screen's geometry.
func captureScreen() (Image, screenGeom, error) {
	switch runtime.GOOS {
	case "darwin":
		return captureScreenDarwin()
	case "windows":
		return captureScreenWindows()
	default:
		return captureScreenLinux()
	}
}

// maxScreenshotDim caps the delivered image so even a Retina capture stays far
// under the API's ~6 MB image limit once re-encoded as JPEG.
const maxScreenshotDim = 1568

// shrinkScreenshot decodes a capture, scales it down to fit maxScreenshotDim,
// and re-encodes it as JPEG. The W/H metadata is always set to the delivered
// dimensions. If decoding fails the original is returned unchanged.
func shrinkScreenshot(img Image) Image {
	src, err := png.Decode(bytes.NewReader(img.Data))
	if err != nil {
		return img
	}
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	scale := 1.0
	if w > maxScreenshotDim || h > maxScreenshotDim {
		if w >= h {
			scale = float64(maxScreenshotDim) / float64(w)
		} else {
			scale = float64(maxScreenshotDim) / float64(h)
		}
	}
	nw, nh := int(float64(w)*scale+0.5), int(float64(h)*scale+0.5)
	if nw < 1 {
		nw = 1
	}
	if nh < 1 {
		nh = 1
	}
	dst := image.NewRGBA(image.Rect(0, 0, nw, nh))
	for y := 0; y < nh; y++ {
		sy := b.Min.Y + int(float64(y)/scale)
		for x := 0; x < nw; x++ {
			dst.Set(x, y, src.At(b.Min.X+int(float64(x)/scale), sy))
		}
	}
	var buf bytes.Buffer
	if jpeg.Encode(&buf, dst, &jpeg.Options{Quality: 75}) != nil {
		return img
	}
	return Image{MIME: "image/jpeg", Data: buf.Bytes(), W: nw, H: nh}
}

func captureScreenDarwin() (Image, screenGeom, error) {
	tmp, err := os.CreateTemp("", "nocturne-shot-*.png")
	if err != nil {
		return Image{}, screenGeom{}, err
	}
	path := tmp.Name()
	tmp.Close()
	defer os.Remove(path)

	if out, err := exec.Command("screencapture", "-x", path).CombinedOutput(); err != nil {
		return Image{}, screenGeom{}, fmt.Errorf("screencapture: %s — screenshots need the Screen Recording permission for your terminal (System Settings → Privacy & Security → Screen Recording)", oneLine(strings.TrimSpace(string(out)), 200))
	}
	data, err := os.ReadFile(path)
	if err != nil || len(data) == 0 {
		return Image{}, screenGeom{}, fmt.Errorf("screencapture produced no image")
	}
	return Image{MIME: "image/png", Data: data}, screenGeomDarwin(), nil
}

// screenGeomDarwin reports the logical (point) size of the main display, which
// is the coordinate space CGEvent mouse actions use.
func screenGeomDarwin() screenGeom {
	script := `ObjC.import('AppKit');
var s = $.NSScreen.mainScreen;
JSON.stringify({w: s.frame.size.width, h: s.frame.size.height});`
	out, err := exec.Command("osascript", "-l", "JavaScript", "-e", script).Output()
	if err != nil {
		return screenGeom{}
	}
	var g struct {
		W float64 `json:"w"`
		H float64 `json:"h"`
	}
	if json.Unmarshal(out, &g) != nil {
		return screenGeom{}
	}
	return screenGeom{W: int(g.W), H: int(g.H)}
}

func captureScreenWindows() (Image, screenGeom, error) {
	tmp, err := os.CreateTemp("", "nocturne-shot-*.png")
	if err != nil {
		return Image{}, screenGeom{}, err
	}
	path := tmp.Name()
	tmp.Close()
	defer os.Remove(path)

	ps := `Add-Type -AssemblyName System.Windows.Forms,System.Drawing;` +
		`$b=[System.Windows.Forms.SystemInformation]::VirtualScreen;` +
		`$bmp=New-Object System.Drawing.Bitmap $b.Width,$b.Height;` +
		`$g=[System.Drawing.Graphics]::FromImage($bmp);` +
		`$g.CopyFromScreen($b.Left,$b.Top,0,0,$bmp.Size);` +
		`$bmp.Save(` + strconv.Quote(path) + `,[System.Drawing.Imaging.ImageFormat]::Png);` +
		`$g.Dispose();$bmp.Dispose();` +
		`Write-Output "$($b.Width) $($b.Height)";`
	out, err := exec.Command("powershell", "-NoProfile", "-Command", ps).CombinedOutput()
	if err != nil {
		return Image{}, screenGeom{}, fmt.Errorf("powershell: %s", oneLine(strings.TrimSpace(string(out)), 200))
	}
	data, err := os.ReadFile(path)
	if err != nil || len(data) == 0 {
		return Image{}, screenGeom{}, fmt.Errorf("screenshot produced no image")
	}
	var geom screenGeom
	if f := strings.Fields(string(out)); len(f) == 2 {
		geom.W, _ = strconv.Atoi(f[0])
		geom.H, _ = strconv.Atoi(f[1])
	}
	return Image{MIME: "image/png", Data: data}, geom, nil
}

func captureScreenLinux() (Image, screenGeom, error) {
	// Native X11 capture first — no external tools needed.
	if img, geom, err := nativeScreen(); err == nil {
		return img, geom, nil
	}

	tmp, err := os.CreateTemp("", "nocturne-shot-*.png")
	if err != nil {
		return Image{}, screenGeom{}, err
	}
	path := tmp.Name()
	tmp.Close()
	defer os.Remove(path)

	candidates := [][]string{
		{"grim", path},                      // Wayland (wlroots)
		{"import", "-window", "root", path}, // ImageMagick, X11
		{"scrot", path},                     // X11
		{"gnome-screenshot", "-f", path},    // GNOME
	}
	var lastErr error
	ok := false
	for _, c := range candidates {
		if _, err := exec.LookPath(c[0]); err != nil {
			lastErr = fmt.Errorf("%s not installed", c[0])
			continue
		}
		if out, err := exec.Command(c[0], c[1:]...).CombinedOutput(); err == nil {
			ok = true
			break
		} else {
			lastErr = fmt.Errorf("%s: %s", c[0], oneLine(strings.TrimSpace(string(out)), 200))
		}
	}
	if !ok {
		return Image{}, screenGeom{}, fmt.Errorf("no working screenshot path (native X11 capture: %v; no grim/scrot/ImageMagick fallback: %v)%s", nativeBackendErr(), lastErr, waylandHint())
	}
	data, err := os.ReadFile(path)
	if err != nil || len(data) == 0 {
		return Image{}, screenGeom{}, fmt.Errorf("screenshot produced no image")
	}
	var geom screenGeom
	if out, err := exec.Command("xdotool", "getdisplaygeometry").Output(); err == nil {
		if f := strings.Fields(string(out)); len(f) == 2 {
			geom.W, _ = strconv.Atoi(f[0])
			geom.H, _ = strconv.Atoi(f[1])
		}
	}
	return Image{MIME: "image/png", Data: data}, geom, nil
}

// --- mouse & keyboard backends ----------------------------------------------

func osClick(x, y int, button string, double bool) error {
	switch runtime.GOOS {
	case "darwin":
		return darwinClick(x, y, button, double)
	case "windows":
		return windowsClick(x, y, button, double)
	default:
		if err := nativeClick(x, y, button, double); err == nil {
			return nil
		}
		return xdotoolClick(x, y, button, double)
	}
}

func osMoveMouse(x, y int) error {
	switch runtime.GOOS {
	case "darwin":
		return darwinMove(x, y)
	case "windows":
		return windowsMouseEvent(x, y, "", 0)
	default:
		if err := nativeMove(x, y); err == nil {
			return nil
		}
		return xdotool("mousemove", strconv.Itoa(x), strconv.Itoa(y))
	}
}

func osScroll(dx, dy int) error {
	switch runtime.GOOS {
	case "darwin":
		// CGEvent wheel1 positive scrolls UP; flip so dy>0 means down.
		script := fmt.Sprintf(`ObjC.import('CoreGraphics');
var ev = $.CGEventCreateScrollWheelEvent(null, $.kCGScrollEventUnitLine, 2, %d, %d);
$.CGEventPost($.kCGHIDEventTap, ev);`, -dy, -dx)
		return runJXA(script)
	case "windows":
		// WHEEL delta: +120 per notch scrolls up; flip so dy>0 means down.
		return windowsMouseEvent(0, 0, "wheel", -120*dy)
	default:
		if err := nativeScroll(dx, dy); err == nil {
			return nil
		}
		btn := "5" // wheel down
		n := abs(dy)
		if dy < 0 {
			btn = "4" // wheel up
		}
		args := []string{}
		for i := 0; i < n; i++ {
			args = append(args, "click", btn)
		}
		if len(args) == 0 {
			return nil
		}
		return xdotool(args...)
	}
}

func osTypeText(text string) error {
	switch runtime.GOOS {
	case "darwin":
		return runJXA("Application('System Events').keystroke(" + jsString(text) + ");")
	case "windows":
		return windowsSendKeys(sendKeysEscape(text))
	default:
		if err := nativeType(text); err == nil {
			return nil
		}
		return xdotool("type", "--", text)
	}
}

func osKeyPress(key string, mods []string) error {
	def, ok := lookupKey(key)
	if !ok {
		return fmt.Errorf("unknown key %q — use a name like enter/tab/escape/up/f1 or a single character", key)
	}
	switch runtime.GOOS {
	case "darwin":
		using := jxaModifiers(mods)
		if def.macCode >= 0 {
			return runJXA(fmt.Sprintf("Application('System Events').keyCode(%d%s);", def.macCode, using))
		}
		return runJXA("Application('System Events').keystroke(" + jsString(key) + using + ");")
	case "windows":
		token := def.winKey
		if token == "" {
			token = sendKeysEscape(key)
		}
		prefix := ""
		for _, m := range mods {
			switch strings.ToLower(m) {
			case "ctrl", "control":
				prefix += "^"
			case "alt", "option":
				prefix += "%"
			case "shift":
				prefix += "+"
			case "cmd", "command", "win", "super":
				return fmt.Errorf("the Windows key is not supported as a modifier on Windows")
			}
		}
		return windowsSendKeys(prefix + token)
	default:
		if err := nativeKeyPress(key, mods); err == nil {
			return nil
		}
		combo := def.x11Key
		if combo == "" {
			combo = key
		}
		parts := []string{}
		for _, m := range mods {
			switch strings.ToLower(m) {
			case "ctrl", "control":
				parts = append(parts, "ctrl")
			case "alt", "option":
				parts = append(parts, "alt")
			case "shift":
				parts = append(parts, "shift")
			case "cmd", "command", "super", "win":
				parts = append(parts, "super")
			}
		}
		parts = append(parts, combo)
		return xdotool("key", strings.Join(parts, "+"))
	}
}

func osOpen(target string) error {
	switch runtime.GOOS {
	case "darwin":
		// App names need -a; paths and URLs go straight to open.
		if !strings.Contains(target, "://") && !strings.ContainsAny(target, `/\`) && !strings.HasPrefix(target, ".") {
			if err := exec.Command("open", "-a", target).Run(); err == nil {
				return nil
			}
		}
		if out, err := exec.Command("open", target).CombinedOutput(); err != nil {
			return fmt.Errorf("open: %s", oneLine(strings.TrimSpace(string(out)), 200))
		}
		return nil
	case "windows":
		if out, err := exec.Command("explorer.exe", target).CombinedOutput(); err != nil {
			return fmt.Errorf("explorer: %s", oneLine(strings.TrimSpace(string(out)), 200))
		}
		return nil
	default:
		var lastErr error
		for _, tool := range [][]string{{"xdg-open", target}, {"gio", "open", target}} {
			if _, err := exec.LookPath(tool[0]); err != nil {
				continue
			}
			if out, err := exec.Command(tool[0], tool[1:]...).CombinedOutput(); err != nil {
				lastErr = fmt.Errorf("%s: %s", tool[0], oneLine(strings.TrimSpace(string(out)), 200))
				continue
			}
			return nil
		}
		// Minimal systems may have no opener at all; if the target names an
		// executable on PATH, launch it directly.
		if !strings.ContainsAny(target, `/\`) {
			if p, err := exec.LookPath(target); err == nil {
				if err := exec.Command(p).Start(); err == nil {
					return nil
				}
			}
		}
		if lastErr != nil {
			return lastErr
		}
		return fmt.Errorf("no way to open %q (no xdg-open, gio, or matching executable)", target)
	}
}

// --- macOS (JXA: CoreGraphics + System Events) -------------------------------

// runJXA executes a JavaScript-for-Automation script.
func runJXA(script string) error {
	out, err := exec.Command("osascript", "-l", "JavaScript", "-e", script).CombinedOutput()
	if err != nil {
		msg := oneLine(strings.TrimSpace(string(out)), 200)
		if msg == "" {
			msg = err.Error()
		}
		return fmt.Errorf("%s%s", msg, accessibilityHint)
	}
	return nil
}

// jsString quotes s as a JavaScript string literal; Go's %q escaping is valid
// JS for the characters that matter (quotes, backslash, control chars).
func jsString(s string) string { return strconv.Quote(s) }

// jxaModifiers renders the ", {using: […]}" argument fragment for System
// Events keystroke/keyCode.
func jxaModifiers(mods []string) string {
	var list []string
	for _, m := range mods {
		switch strings.ToLower(strings.TrimSpace(m)) {
		case "cmd", "command":
			list = append(list, `"command down"`)
		case "ctrl", "control":
			list = append(list, `"control down"`)
		case "alt", "option":
			list = append(list, `"option down"`)
		case "shift":
			list = append(list, `"shift down"`)
		}
	}
	if len(list) == 0 {
		return ""
	}
	return ", {using: [" + strings.Join(list, ", ") + "]}"
}

func darwinMove(x, y int) error {
	script := fmt.Sprintf(`ObjC.import('CoreGraphics');
var ev = $.CGEventCreateMouseEvent(null, $.kCGEventMouseMoved, {x: %d, y: %d}, $.kCGMouseButtonLeft);
$.CGEventPost($.kCGHIDEventTap, ev);`, x, y)
	return runJXA(script)
}

func darwinClick(x, y int, button string, double bool) error {
	down, up, btn := "$.kCGEventLeftMouseDown", "$.kCGEventLeftMouseUp", "$.kCGMouseButtonLeft"
	switch button {
	case "right":
		down, up, btn = "$.kCGEventRightMouseDown", "$.kCGEventRightMouseUp", "$.kCGMouseButtonRight"
	case "middle":
		down, up, btn = "$.kCGEventOtherMouseDown", "$.kCGEventOtherMouseUp", "$.kCGMouseButtonCenter"
	}
	clicks := 1
	if double {
		clicks = 2
	}
	script := fmt.Sprintf(`ObjC.import('CoreGraphics');
var p = {x: %d, y: %d};
var mv = $.CGEventCreateMouseEvent(null, $.kCGEventMouseMoved, p, %s);
$.CGEventPost($.kCGHIDEventTap, mv);
for (var n = 1; n <= %d; n++) {
  var dn = $.CGEventCreateMouseEvent(null, %s, p, %s);
  $.CGEventSetIntegerValueField(dn, $.kCGMouseEventClickState, n);
  $.CGEventPost($.kCGHIDEventTap, dn);
  var up = $.CGEventCreateMouseEvent(null, %s, p, %s);
  $.CGEventSetIntegerValueField(up, $.kCGMouseEventClickState, n);
  $.CGEventPost($.kCGHIDEventTap, up);
}`, x, y, btn, clicks, down, btn, up, btn)
	return runJXA(script)
}

// --- Windows (PowerShell pinvoke) --------------------------------------------

const windowsUser32Type = `
Add-Type -Name U32 -Namespace NocturneWin -MemberDefinition '
[System.Runtime.InteropServices.DllImport("user32.dll")] public static extern bool SetCursorPos(int X, int Y);
[System.Runtime.InteropServices.DllImport("user32.dll")] public static extern void mouse_event(uint f, uint x, uint y, uint d, int e);
';`

// windowsMouseEvent moves the pointer and/or fires one mouse_event. action is
// "" (move only), "left"/"right"/"middle" (down+up), or "wheel" (data=delta).
func windowsMouseEvent(x, y int, action string, data int) error {
	var b strings.Builder
	b.WriteString(windowsUser32Type)
	if x != 0 || y != 0 {
		fmt.Fprintf(&b, "[NocturneWin.U32]::SetCursorPos(%d, %d) | Out-Null;", x, y)
	}
	switch action {
	case "left":
		b.WriteString("[NocturneWin.U32]::mouse_event(0x0002,0,0,0,0);[NocturneWin.U32]::mouse_event(0x0004,0,0,0,0);")
	case "right":
		b.WriteString("[NocturneWin.U32]::mouse_event(0x0008,0,0,0,0);[NocturneWin.U32]::mouse_event(0x0010,0,0,0,0);")
	case "middle":
		b.WriteString("[NocturneWin.U32]::mouse_event(0x0020,0,0,0,0);[NocturneWin.U32]::mouse_event(0x0040,0,0,0,0);")
	case "wheel":
		fmt.Fprintf(&b, "[NocturneWin.U32]::mouse_event(0x0800,0,0,%d,0);", data)
	}
	out, err := exec.Command("powershell", "-NoProfile", "-Command", b.String()).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s", oneLine(strings.TrimSpace(string(out)), 200))
	}
	return nil
}

func windowsClick(x, y int, button string, double bool) error {
	if err := windowsMouseEvent(x, y, button, 0); err != nil {
		return err
	}
	if double {
		return windowsMouseEvent(x, y, button, 0)
	}
	return nil
}

// sendKeysEscape wraps SendKeys' metacharacters so they type literally.
func sendKeysEscape(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch r {
		case '+', '^', '%', '~', '(', ')', '[', ']', '{', '}':
			b.WriteByte('{')
			b.WriteRune(r)
			b.WriteByte('}')
		case '\n':
			b.WriteString("{ENTER}")
		case '\t':
			b.WriteString("{TAB}")
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

func windowsSendKeys(keys string) error {
	ps := `Add-Type -AssemblyName System.Windows.Forms;` +
		`[System.Windows.Forms.SendKeys]::SendWait(` + strconv.Quote(keys) + `);`
	out, err := exec.Command("powershell", "-NoProfile", "-Command", ps).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s", oneLine(strings.TrimSpace(string(out)), 200))
	}
	return nil
}

// --- Linux (xdotool) ---------------------------------------------------------

// waylandHint explains the common native-Wayland failure case when neither
// the native X11 backend nor xdotool can drive the GUI.
func waylandHint() string {
	if os.Getenv("WAYLAND_DISPLAY") != "" && os.Getenv("DISPLAY") == "" {
		return " — this looks like a native Wayland session with no XWayland; native Wayland input is not supported yet, run under an X11/XWayland session"
	}
	return ""
}

func xdotool(args ...string) error {
	if _, err := exec.LookPath("xdotool"); err != nil {
		return fmt.Errorf("GUI control unavailable (native X11 backend: %v; xdotool not installed either)%s", nativeBackendErr(), waylandHint())
	}
	if out, err := exec.Command("xdotool", args...).CombinedOutput(); err != nil {
		return fmt.Errorf("xdotool %s: %s", args[0], oneLine(strings.TrimSpace(string(out)), 200))
	}
	return nil
}

func xdotoolClick(x, y int, button string, double bool) error {
	btn := "1"
	switch button {
	case "right":
		btn = "3"
	case "middle":
		btn = "2"
	}
	args := []string{"mousemove", strconv.Itoa(x), strconv.Itoa(y)}
	if double {
		args = append(args, "click", "--repeat", "2", btn)
	} else {
		args = append(args, "click", btn)
	}
	return xdotool(args...)
}

// --- key name tables ---------------------------------------------------------

// keyDef holds the platform spellings of one named key. macCode -1 means the
// key has no hardware code and should be typed as a character instead.
type keyDef struct {
	macCode int
	winKey  string // SendKeys token ("" = use the literal character)
	x11Key  string // xdotool keysym ("" = use the literal character)
}

var keyNames = map[string]keyDef{
	"enter":     {36, "{ENTER}", "Return"},
	"return":    {36, "{ENTER}", "Return"},
	"tab":       {48, "{TAB}", "Tab"},
	"escape":    {53, "{ESC}", "Escape"},
	"esc":       {53, "{ESC}", "Escape"},
	"space":     {49, " ", "space"},
	"backspace": {51, "{BACKSPACE}", "BackSpace"},
	"delete":    {117, "{DELETE}", "Delete"},
	"up":        {126, "{UP}", "Up"},
	"down":      {125, "{DOWN}", "Down"},
	"left":      {123, "{LEFT}", "Left"},
	"right":     {124, "{RIGHT}", "Right"},
	"home":      {115, "{HOME}", "Home"},
	"end":       {119, "{END}", "End"},
	"pageup":    {116, "{PGUP}", "Page_Up"},
	"pagedown":  {121, "{PGDN}", "Page_Down"},
	"f1":        {122, "{F1}", "F1"},
	"f2":        {120, "{F2}", "F2"},
	"f3":        {99, "{F3}", "F3"},
	"f4":        {118, "{F4}", "F4"},
	"f5":        {96, "{F5}", "F5"},
	"f6":        {97, "{F6}", "F6"},
	"f7":        {98, "{F7}", "F7"},
	"f8":        {100, "{F8}", "F8"},
	"f9":        {101, "{F9}", "F9"},
	"f10":       {109, "{F10}", "F10"},
	"f11":       {103, "{F11}", "F11"},
	"f12":       {111, "{F12}", "F12"},
}

// lookupKey resolves a model-supplied key name. A single printable character
// is always valid and is typed as a character.
func lookupKey(key string) (keyDef, bool) {
	if def, ok := keyNames[key]; ok {
		return def, true
	}
	if len([]rune(key)) == 1 {
		return keyDef{macCode: -1}, true
	}
	return keyDef{}, false
}
