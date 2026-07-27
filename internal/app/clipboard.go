package app

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"unicode"
	"unicode/utf8"
)

var imageExts = map[string]string{
	".png":  "image/png",
	".jpg":  "image/jpeg",
	".jpeg": "image/jpeg",
	".gif":  "image/gif",
	".webp": "image/webp",
	".bmp":  "image/bmp",
}

// grabClipboardImage pulls an image off the system clipboard, returning a
// descriptive error if there is no image or the platform tools are missing.
func grabClipboardImage() (Image, error) {
	switch runtime.GOOS {
	case "darwin":
		return grabDarwin()
	case "windows":
		return grabWindows()
	default:
		return grabLinux()
	}
}

func grabDarwin() (Image, error) {
	// Prefer pngpaste if installed (fast, clean).
	if path, err := exec.LookPath("pngpaste"); err == nil {
		out, err := exec.Command(path, "-").Output()
		if err == nil && len(out) > 0 {
			return Image{MIME: "image/png", Data: out}, nil
		}
	}

	// Fall back to AppKit via JXA. macOS often puts screenshots on the
	// pasteboard as TIFF/NSImage data rather than raw PNG. Coercing with
	// AppleScript's `the clipboard as «class PNGf»` can therefore fail with
	// "Can't make some data into the expected type" (-2700). AppKit reads any
	// pasteboard image representation and re-encodes it as PNG.
	tmp, err := os.CreateTemp("", "nocturne-*.png")
	if err != nil {
		return Image{}, err
	}
	tmpPath := tmp.Name()
	tmp.Close()
	defer os.Remove(tmpPath)

	script := fmt.Sprintf(`ObjC.import('AppKit');
ObjC.import('Foundation');

const img = $.NSImage.alloc.initWithPasteboard($.NSPasteboard.generalPasteboard);
if (!img) {
	throw new Error('no image on clipboard');
}

const tiff = img.TIFFRepresentation;
if (!tiff) {
	throw new Error('no image on clipboard');
}

const rep = $.NSBitmapImageRep.imageRepWithData(tiff);
if (!rep) {
	throw new Error('could not decode clipboard image');
}

const png = rep.representationUsingTypeProperties(4, $());
if (!png) {
	throw new Error('could not encode clipboard image as PNG');
}

if (!png.writeToFileAtomically(%q, true)) {
	throw new Error('could not write clipboard image');
}`, tmpPath)

	cmd := exec.Command("osascript", "-l", "JavaScript", "-e", script)
	if out, err := cmd.CombinedOutput(); err != nil {
		msg := strings.TrimSpace(string(out))
		if msg == "" {
			msg = err.Error()
		}
		return Image{}, fmt.Errorf("no image on clipboard (%s)", msg)
	}
	data, err := os.ReadFile(tmpPath)
	if err != nil || len(data) == 0 {
		return Image{}, fmt.Errorf("no image on clipboard")
	}
	return Image{MIME: "image/png", Data: data}, nil
}

func grabLinux() (Image, error) {
	// Wayland first, then X11.
	candidates := [][]string{
		{"wl-paste", "-t", "image/png"},
		{"xclip", "-selection", "clipboard", "-t", "image/png", "-o"},
	}
	var lastErr error
	for _, c := range candidates {
		if _, err := exec.LookPath(c[0]); err != nil {
			lastErr = fmt.Errorf("%s not installed", c[0])
			continue
		}
		out, err := exec.Command(c[0], c[1:]...).Output()
		if err == nil && len(out) > 0 {
			return Image{MIME: "image/png", Data: out}, nil
		}
		lastErr = err
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no image on clipboard")
	}
	return Image{}, fmt.Errorf("could not read clipboard image (install wl-clipboard or xclip): %v", lastErr)
}

func grabWindows() (Image, error) {
	ps := `Add-Type -AssemblyName System.Windows.Forms,System.Drawing;` +
		`$img=[System.Windows.Forms.Clipboard]::GetImage();` +
		`if($img -eq $null){exit 1};` +
		`$ms=New-Object System.IO.MemoryStream;` +
		`$img.Save($ms,[System.Drawing.Imaging.ImageFormat]::Png);` +
		`$out=[Console]::OpenStandardOutput();` +
		`$bytes=$ms.ToArray();$out.Write($bytes,0,$bytes.Length);$out.Flush();`
	cmd := exec.Command("powershell", "-NoProfile", "-Command", ps)
	var buf bytes.Buffer
	cmd.Stdout = &buf
	if err := cmd.Run(); err != nil || buf.Len() == 0 {
		return Image{}, fmt.Errorf("no image on clipboard")
	}
	return Image{MIME: "image/png", Data: buf.Bytes()}, nil
}

// copyText writes s to the system clipboard, returning an error if no clipboard
// tool is available. Used by the dashboard's ctrl+s "copy summary".
func copyText(s string) error {
	// clip.exe decodes stdin as the console's OEM code page, so piping UTF-8 to
	// it mojibakes any non-ASCII (·, →, █, ✓, …). Go through PowerShell reading a
	// UTF-8 temp file instead, which puts real Unicode on the clipboard.
	if runtime.GOOS == "windows" {
		return copyTextWindows(s)
	}

	var name string
	var args []string
	switch runtime.GOOS {
	case "darwin":
		name = "pbcopy"
	default:
		if _, err := exec.LookPath("wl-copy"); err == nil {
			name = "wl-copy"
		} else {
			name, args = "xclip", []string{"-selection", "clipboard"}
		}
	}
	if _, err := exec.LookPath(name); err != nil {
		return fmt.Errorf("%s not found", name)
	}
	cmd := exec.Command(name, args...)
	cmd.Stdin = strings.NewReader(s)
	return cmd.Run()
}

// copyTextWindows writes s to the clipboard as proper Unicode by staging it in a
// UTF-8 temp file and letting PowerShell's Set-Clipboard read it back.
func copyTextWindows(s string) error {
	tmp, err := os.CreateTemp("", "nocturne-clip-*.txt")
	if err != nil {
		return err
	}
	path := tmp.Name()
	defer os.Remove(path)
	if _, err := tmp.WriteString(s); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	ps := "Get-Content -Raw -Encoding UTF8 -LiteralPath " + psSingleQuote(path) + " | Set-Clipboard"
	return exec.Command("powershell", "-NoProfile", "-Command", ps).Run()
}

// psSingleQuote wraps s in a PowerShell single-quoted string literal, doubling
// any embedded single quotes so the value is passed through verbatim.
func psSingleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// loadImageFile reads an image from disk and infers its MIME type.
func loadImageFile(path string) (Image, error) {
	mime, ok := imageExts[strings.ToLower(filepath.Ext(path))]
	if !ok {
		return Image{}, fmt.Errorf("%s is not a recognised image", path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return Image{}, err
	}
	return Image{MIME: mime, Data: data}, nil
}

// extractInlineImages scans submitted text for tokens that point at image
// files on disk (optionally prefixed with @), attaches them, and returns the
// text with those tokens removed while preserving all other whitespace. In
// particular, keep user-entered newlines intact: those are part of the prompt
// and should be echoed/sent exactly as typed.
func extractInlineImages(text, workdir string) (string, []Image) {
	var images []Image
	var out strings.Builder

	for i := 0; i < len(text); {
		r, size := utf8.DecodeRuneInString(text[i:])
		if r == utf8.RuneError && size == 0 {
			break
		}
		if !unicode.IsSpace(r) {
			start := i
			i += size
			for i < len(text) {
				r, size = utf8.DecodeRuneInString(text[i:])
				if unicode.IsSpace(r) {
					break
				}
				i += size
			}

			field := text[start:i]
			token := strings.TrimPrefix(field, "@")
			token = strings.Trim(token, `"'`)
			if _, ok := imageExts[strings.ToLower(filepath.Ext(token))]; ok {
				p := token
				if !filepath.IsAbs(p) {
					p = filepath.Join(workdir, p)
				}
				if img, err := loadImageFile(p); err == nil {
					images = append(images, img)
					continue
				}
			}
			out.WriteString(field)
			continue
		}

		out.WriteString(text[i : i+size])
		i += size
	}

	return strings.TrimSpace(out.String()), images
}
