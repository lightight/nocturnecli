package app

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// defaultUpdateBase is the first-party Nocturne distribution host used for
// update checks and binary downloads. Override with NOCTURNE_UPDATE_BASE for
// staging/local testing.
const defaultUpdateBase = "https://nocturnecode.lol"

// defaultRepo is retained for the shell installers' GitHub fallback. The CLI
// updater itself checks nocturnecode.lol first and downloads from /bin/ there.
const defaultRepo = "lightight/nocturnecli"

func updateBaseURL() string {
	if b := strings.TrimRight(strings.TrimSpace(os.Getenv("NOCTURNE_UPDATE_BASE")), "/"); b != "" {
		return b
	}
	return defaultUpdateBase
}

func repoSlug() string {
	if r := strings.TrimSpace(os.Getenv("NOCTURNE_REPO")); r != "" {
		return r
	}
	return defaultRepo
}

// assetName is the release asset for the current platform, matching the names
// produced by the release workflow / Makefile dist target and served from
// nocturnecode.lol/bin/.
func assetName() string {
	name := fmt.Sprintf("nocturne_%s_%s", runtime.GOOS, runtime.GOARCH)
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	return name
}

func normVersion(v string) string { return strings.TrimPrefix(strings.TrimSpace(v), "v") }

type updateInfo struct {
	Version string `json:"version"`
	URL     string `json:"url"`
}

type hostedUpdate struct {
	Version  string `json:"version"`
	Tag      string `json:"tag,omitempty"`
	TagName  string `json:"tag_name,omitempty"`
	Asset    string `json:"asset,omitempty"`
	URL      string `json:"url,omitempty"`
	AssetURL string `json:"asset_url,omitempty"`
}

func (u hostedUpdate) latest() string {
	for _, v := range []string{u.Version, u.Tag, u.TagName} {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func (u hostedUpdate) binaryURL(base string) string {
	for _, candidate := range []string{u.AssetURL, u.URL, u.Asset} {
		candidate = strings.TrimSpace(candidate)
		if candidate == "" {
			continue
		}
		if strings.HasPrefix(candidate, "http://") || strings.HasPrefix(candidate, "https://") {
			return candidate
		}
		return base + "/" + strings.TrimLeft(candidate, "/")
	}
	return base + "/bin/" + assetName()
}

// compareVersions compares dotted semver-ish versions after stripping a leading
// v. It returns -1, 0, or 1. Numeric components compare numerically, so 0.10.0
// correctly sorts after 0.9.0; any suffix falls back to lexical comparison.
func compareVersions(a, b string) int {
	a = normVersion(a)
	b = normVersion(b)
	if a == b {
		return 0
	}
	as := strings.FieldsFunc(a, func(r rune) bool { return r == '.' || r == '-' || r == '_' })
	bs := strings.FieldsFunc(b, func(r rune) bool { return r == '.' || r == '-' || r == '_' })
	max := len(as)
	if len(bs) > max {
		max = len(bs)
	}
	for i := 0; i < max; i++ {
		av, bv := "0", "0"
		if i < len(as) {
			av = as[i]
		}
		if i < len(bs) {
			bv = bs[i]
		}
		ai, aerr := strconv.Atoi(av)
		bi, berr := strconv.Atoi(bv)
		if aerr == nil && berr == nil {
			switch {
			case ai < bi:
				return -1
			case ai > bi:
				return 1
			}
			continue
		}
		if av < bv {
			return -1
		}
		if av > bv {
			return 1
		}
	}
	return strings.Compare(a, b)
}

// checkHostedUpdate asks nocturnecode.lol for update metadata. Newer Nocturne
// servers expose /update.json; /version is accepted as a tiny fallback. If a
// currently-deployed server lacks those endpoints, we fail rather than silently
// checking another host, so /update is genuinely backed by nocturnecode.lol.
func checkHostedUpdate() (updateInfo, error) {
	base := updateBaseURL()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	if info, err := fetchUpdateJSON(ctx, base); err == nil {
		return info, nil
	}
	return fetchVersionText(ctx, base)
}

func fetchUpdateJSON(ctx context.Context, base string) (updateInfo, error) {
	url := base + "/update.json"
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "nocturne-cli")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return updateInfo{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return updateInfo{}, fmt.Errorf("%s returned HTTP %d", url, resp.StatusCode)
	}

	var u hostedUpdate
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&u); err != nil {
		return updateInfo{}, err
	}
	latest := u.latest()
	if latest == "" {
		return updateInfo{}, fmt.Errorf("%s has no version", url)
	}
	return updateInfo{Version: latest, URL: u.binaryURL(base)}, nil
}

func fetchVersionText(ctx context.Context, base string) (updateInfo, error) {
	url := base + "/version"
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	req.Header.Set("Accept", "text/plain")
	req.Header.Set("User-Agent", "nocturne-cli")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return updateInfo{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return updateInfo{}, fmt.Errorf("%s returned HTTP %d", url, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return updateInfo{}, err
	}
	latest := strings.TrimSpace(string(body))
	if latest == "" {
		return updateInfo{}, fmt.Errorf("%s is empty", url)
	}
	return updateInfo{Version: latest, URL: base + "/bin/" + assetName()}, nil
}

// doUpdate checks nocturnecode.lol for the latest release and, unless checkOnly,
// replaces the running binary with it. It returns a human-readable status line.
func doUpdate(checkOnly bool) (string, error) {
	info, err := checkHostedUpdate()
	if err != nil {
		return "", err
	}
	latest := info.Version
	if compareVersions(Version, latest) >= 0 {
		return fmt.Sprintf("Already on the latest version (%s).", Version), nil
	}
	if checkOnly {
		return fmt.Sprintf("Update available: %s → %s. Run `nocturne update`.", Version, latest), nil
	}

	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}

	tmp, err := downloadTo(info.URL, filepath.Dir(exe))
	if err != nil {
		return "", err
	}
	if err := replaceExecutable(tmp, exe); err != nil {
		_ = os.Remove(tmp)
		return "", fmt.Errorf("%w (try re-running with sufficient permissions)", err)
	}
	return fmt.Sprintf("Updated %s → %s. Restart nocturne to use it.", Version, latest), nil
}

// downloadTo streams url into a temp file in dir (same filesystem as the target,
// so the later rename is atomic) and returns its path.
func downloadTo(url, dir string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	req.Header.Set("User-Agent", "nocturne-cli")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download failed (%d) for %s", resp.StatusCode, url)
	}

	tmp, err := os.CreateTemp(dir, ".nocturne-update-*")
	if err != nil {
		return "", err
	}
	_, err = io.Copy(tmp, resp.Body)
	tmp.Close()
	if err != nil {
		_ = os.Remove(tmp.Name())
		return "", err
	}
	if err := os.Chmod(tmp.Name(), 0o755); err != nil {
		_ = os.Remove(tmp.Name())
		return "", err
	}
	return tmp.Name(), nil
}

// replaceExecutable swaps newPath in for exe. On Windows a running binary can't
// be overwritten, so the old one is moved aside first.
func replaceExecutable(newPath, exe string) error {
	if runtime.GOOS == "windows" {
		old := exe + ".old"
		_ = os.Remove(old)
		if err := os.Rename(exe, old); err != nil {
			return err
		}
		if err := os.Rename(newPath, exe); err != nil {
			_ = os.Rename(old, exe) // roll back
			return err
		}
		_ = os.Remove(old)
		return nil
	}
	return os.Rename(newPath, exe)
}
