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
	"strings"
	"time"
)

// defaultRepo is the GitHub owner/repo the updater pulls releases from. It can
// be overridden with NOCTURNE_REPO (handy for forks).
const defaultRepo = "lightight/nocturnecli"

func repoSlug() string {
	if r := strings.TrimSpace(os.Getenv("NOCTURNE_REPO")); r != "" {
		return r
	}
	return defaultRepo
}

// assetName is the release asset for the current platform, matching the names
// produced by the release workflow / Makefile dist target.
func assetName() string {
	name := fmt.Sprintf("nocturne_%s_%s", runtime.GOOS, runtime.GOARCH)
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	return name
}

func normVersion(v string) string { return strings.TrimPrefix(strings.TrimSpace(v), "v") }

// doUpdate checks GitHub for the latest release and, unless checkOnly, replaces
// the running binary with it. It returns a human-readable status line.
func doUpdate(checkOnly bool) (string, error) {
	repo := repoSlug()

	latest, err := latestReleaseTag(repo)
	if err != nil {
		return "", err
	}
	if normVersion(latest) == normVersion(Version) {
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

	url := fmt.Sprintf("https://github.com/%s/releases/download/%s/%s", repo, latest, assetName())
	tmp, err := downloadTo(url, filepath.Dir(exe))
	if err != nil {
		return "", err
	}
	if err := replaceExecutable(tmp, exe); err != nil {
		_ = os.Remove(tmp)
		return "", fmt.Errorf("%w (try re-running with sufficient permissions)", err)
	}
	return fmt.Sprintf("Updated %s → %s. Restart nocturne to use it.", Version, latest), nil
}

// latestReleaseTag asks the GitHub API for the newest published release tag.
func latestReleaseTag(repo string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	url := fmt.Sprintf("https://api.github.com/repos/%s/releases/latest", repo)
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "nocturne-cli")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return "", fmt.Errorf("no published releases found for %s", repo)
	}
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("GitHub API %d while checking releases", resp.StatusCode)
	}

	var rel struct {
		TagName string `json:"tag_name"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return "", err
	}
	if rel.TagName == "" {
		return "", fmt.Errorf("latest release has no tag")
	}
	return rel.TagName, nil
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
