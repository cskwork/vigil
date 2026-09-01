package browser

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"time"
)

// DetectChromium finds a Chromium-compatible binary for this machine:
// newest Playwright cache entry (headless shell preferred, then full Chromium),
// then well-known system browsers, then PATH.
func DetectChromium() (string, error) {
	var tried []string
	for _, root := range playwrightRoots() {
		if p := newestPlaywright(root, "chromium_headless_shell-*", headlessShellRelPaths()); p != "" {
			return p, nil
		}
		if p := newestPlaywright(root, "chromium-*", chromiumRelPaths()); p != "" {
			return p, nil
		}
		tried = append(tried, root)
	}
	for _, p := range systemBrowsers() {
		if isExecutable(p) {
			return p, nil
		}
		tried = append(tried, p)
	}
	for _, name := range []string{"google-chrome", "google-chrome-stable", "chromium", "chromium-browser", "chrome"} {
		if p, err := exec.LookPath(name); err == nil {
			return p, nil
		}
	}
	return "", fmt.Errorf("no Chromium binary found (looked in %v and PATH); install Playwright browsers (npx playwright install chromium) or set browser.chromium.binary", tried)
}

func playwrightRoots() []string {
	var roots []string
	if p := os.Getenv("PLAYWRIGHT_BROWSERS_PATH"); p != "" && p != "0" {
		roots = append(roots, p)
	}
	home, _ := os.UserHomeDir()
	switch runtime.GOOS {
	case "darwin":
		roots = append(roots, filepath.Join(home, "Library", "Caches", "ms-playwright"))
	case "linux":
		if x := os.Getenv("XDG_CACHE_HOME"); x != "" {
			roots = append(roots, filepath.Join(x, "ms-playwright"))
		}
		roots = append(roots, filepath.Join(home, ".cache", "ms-playwright"))
	case "windows":
		roots = append(roots, filepath.Join(os.Getenv("LOCALAPPDATA"), "ms-playwright"))
	}
	return roots
}

func headlessShellRelPaths() []string {
	switch runtime.GOOS {
	case "darwin":
		if runtime.GOARCH == "arm64" {
			return []string{"chrome-headless-shell-mac-arm64/chrome-headless-shell", "chrome-headless-shell-mac/chrome-headless-shell"}
		}
		return []string{"chrome-headless-shell-mac/chrome-headless-shell", "chrome-headless-shell-mac-arm64/chrome-headless-shell"}
	case "linux":
		if runtime.GOARCH == "arm64" {
			return []string{"chrome-headless-shell-linux-arm64/chrome-headless-shell", "chrome-headless-shell-linux/chrome-headless-shell"}
		}
		return []string{"chrome-headless-shell-linux/chrome-headless-shell"}
	case "windows":
		return []string{"chrome-headless-shell-win64/chrome-headless-shell.exe"}
	}
	return nil
}

func chromiumRelPaths() []string {
	switch runtime.GOOS {
	case "darwin":
		mac := "chrome-mac/Google Chrome for Testing.app/Contents/MacOS/Google Chrome for Testing"
		arm := "chrome-mac-arm64/Google Chrome for Testing.app/Contents/MacOS/Google Chrome for Testing"
		legacy := "chrome-mac/Chromium.app/Contents/MacOS/Chromium"
		if runtime.GOARCH == "arm64" {
			return []string{arm, mac, legacy}
		}
		return []string{mac, arm, legacy}
	case "linux":
		if runtime.GOARCH == "arm64" {
			return []string{"chrome-linux-arm64/chrome", "chrome-linux/chrome"}
		}
		return []string{"chrome-linux/chrome"}
	case "windows":
		return []string{"chrome-win64/chrome.exe", "chrome-win/chrome.exe"}
	}
	return nil
}

func systemBrowsers() []string {
	switch runtime.GOOS {
	case "darwin":
		return []string{
			"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
			"/Applications/Chromium.app/Contents/MacOS/Chromium",
			"/Applications/Google Chrome Canary.app/Contents/MacOS/Google Chrome Canary",
			"/Applications/Microsoft Edge.app/Contents/MacOS/Microsoft Edge",
		}
	case "linux":
		return []string{"/usr/bin/google-chrome", "/usr/bin/google-chrome-stable", "/usr/bin/chromium", "/usr/bin/chromium-browser", "/snap/bin/chromium"}
	case "windows":
		return []string{
			filepath.Join(os.Getenv("ProgramFiles"), "Google", "Chrome", "Application", "chrome.exe"),
			filepath.Join(os.Getenv("ProgramFiles(x86)"), "Google", "Chrome", "Application", "chrome.exe"),
		}
	}
	return nil
}

var revisionRe = regexp.MustCompile(`-(\d+)$`)

// newestPlaywright returns the highest-revision entry under root matching pattern
// that contains one of relPaths.
func newestPlaywright(root, pattern string, relPaths []string) string {
	dirs, _ := filepath.Glob(filepath.Join(root, pattern))
	type cand struct {
		rev  int
		path string
	}
	var cands []cand
	for _, d := range dirs {
		m := revisionRe.FindStringSubmatch(filepath.Base(d))
		if m == nil {
			continue
		}
		rev, _ := strconv.Atoi(m[1])
		for _, rel := range relPaths {
			p := filepath.Join(d, filepath.FromSlash(rel))
			if isExecutable(p) {
				cands = append(cands, cand{rev, p})
				break
			}
		}
	}
	if len(cands) == 0 {
		return ""
	}
	sort.Slice(cands, func(i, j int) bool { return cands[i].rev > cands[j].rev })
	return cands[0].path
}

func isExecutable(p string) bool {
	st, err := os.Stat(p)
	if err != nil || st.IsDir() {
		return false
	}
	if runtime.GOOS == "windows" {
		return true
	}
	return st.Mode()&0o111 != 0
}

// LightpandaNightlyURL is the download URL for this platform's nightly build.
func LightpandaNightlyURL() (string, error) {
	var arch, osName string
	switch runtime.GOARCH {
	case "arm64":
		arch = "aarch64"
	case "amd64":
		arch = "x86_64"
	default:
		return "", fmt.Errorf("lightpanda: unsupported arch %s", runtime.GOARCH)
	}
	switch runtime.GOOS {
	case "darwin":
		osName = "macos"
	case "linux":
		osName = "linux"
	default:
		return "", fmt.Errorf("lightpanda: unsupported OS %s", runtime.GOOS)
	}
	return fmt.Sprintf("https://github.com/lightpanda-io/browser/releases/download/nightly/lightpanda-%s-%s", arch, osName), nil
}

// EnsureLightpandaBinary returns path when it is an executable file, otherwise
// downloads the nightly build for this platform to path (chmod +x) and returns it.
func EnsureLightpandaBinary(path string) (string, error) {
	if path == "" {
		path = "bin/lightpanda"
	}
	path = resolveBinary(path)
	if isExecutable(path) {
		return path, nil
	}
	url, err := LightpandaNightlyURL()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", fmt.Errorf("lightpanda: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("lightpanda: download %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("lightpanda: download %s: HTTP %d", url, resp.StatusCode)
	}
	tmp := path + ".part"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
	if err != nil {
		return "", err
	}
	n, err := io.Copy(f, resp.Body)
	f.Close()
	if err != nil || n == 0 {
		os.Remove(tmp)
		if err == nil {
			err = fmt.Errorf("empty body")
		}
		return "", fmt.Errorf("lightpanda: download %s: %w", url, err)
	}
	if err := os.Chmod(tmp, 0o755); err != nil {
		os.Remove(tmp)
		return "", err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return "", err
	}
	return path, nil
}
