package app

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
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
	// Fall back to AppleScript writing the clipboard PNG to a temp file.
	tmp, err := os.CreateTemp("", "nocturne-*.png")
	if err != nil {
		return Image{}, err
	}
	tmpPath := tmp.Name()
	tmp.Close()
	defer os.Remove(tmpPath)

	script := fmt.Sprintf(`set p to POSIX file %q
set f to (open for access p with write permission)
try
	set d to (the clipboard as «class PNGf»)
	write d to f
	close access f
on error errMsg
	close access f
	error errMsg
end try`, tmpPath)

	cmd := exec.Command("osascript", "-e", script)
	if out, err := cmd.CombinedOutput(); err != nil {
		return Image{}, fmt.Errorf("no image on clipboard (%s)", strings.TrimSpace(string(out)))
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
// text with those tokens removed.
func extractInlineImages(text, workdir string) (string, []Image) {
	fields := strings.Fields(text)
	var images []Image
	var kept []string
	for _, f := range fields {
		token := strings.TrimPrefix(f, "@")
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
		kept = append(kept, f)
	}
	return strings.Join(kept, " "), images
}
