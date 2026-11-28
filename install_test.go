package main

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// fakeGitHub serves canned bodies by URL without touching the network.
type fakeGitHub map[string][]byte

func (f fakeGitHub) RoundTrip(r *http.Request) (*http.Response, error) {
	body, ok := f[r.URL.String()]
	status := http.StatusOK
	if !ok {
		status = http.StatusNotFound
	}
	return &http.Response{StatusCode: status, Body: io.NopCloser(bytes.NewReader(body)), ContentLength: int64(len(body)), Header: http.Header{}, Request: r}, nil
}

// sf19Assets is the real asset list of the official sf_19 release.
var sf19Assets = []string{
	"stockfish-android-arm64-universal.tar.gz",
	"stockfish-android-armv7-neon.tar.gz",
	"stockfish-linux-arm64-universal.tar.gz",
	"stockfish-linux-riscv64-universal.tar.gz",
	"stockfish-linux-x86-64-universal.tar.gz",
	"stockfish-macos-universal.tar.gz",
	"stockfish-windows-arm64-universal.zip",
	"stockfish-windows-x86-64-universal.zip",
}

func TestStockfishAsset(t *testing.T) {
	for _, tc := range []struct{ goos, goarch, want string }{
		{"darwin", "arm64", "stockfish-macos-universal.tar.gz"},
		{"darwin", "amd64", "stockfish-macos-universal.tar.gz"},
		{"linux", "amd64", "stockfish-linux-x86-64-universal.tar.gz"},
		{"linux", "arm64", "stockfish-linux-arm64-universal.tar.gz"},
		{"windows", "amd64", "stockfish-windows-x86-64-universal.zip"},
		{"windows", "arm64", "stockfish-windows-arm64-universal.zip"},
		{"linux", "riscv64", "stockfish-linux-riscv64-universal.tar.gz"},
	} {
		i := stockfishAsset(sf19Assets, tc.goos, tc.goarch)
		if i < 0 || sf19Assets[i] != tc.want {
			t.Errorf("%s/%s: got index %d, want %s", tc.goos, tc.goarch, i, tc.want)
		}
	}
	if i := stockfishAsset([]string{"stockfish-linux-x86-64-universal.tar.gz"}, "darwin", "arm64"); i != -1 {
		t.Errorf("darwin matched a Linux build at index %d", i)
	}
	for _, goarch := range []string{"386", "arm", "ppc64le", "s390x"} {
		if i := stockfishAsset(sf19Assets, "linux", goarch); i != -1 {
			t.Errorf("linux/%s matched %s, want no build", goarch, sf19Assets[i])
		}
	}
}

func TestInstallEngineVerifiesDigest(t *testing.T) {
	var archive bytes.Buffer
	z := zip.NewWriter(&archive)
	w, _ := z.Create("stockfish/stockfish")
	w.Write([]byte("fake engine"))
	z.Close()
	sum := sha256.Sum256(archive.Bytes())
	good := "sha256:" + hex.EncodeToString(sum[:])

	i := stockfishAsset(sf19Assets, runtime.GOOS, runtime.GOARCH)
	if i < 0 {
		t.Skipf("no sf_19 build for %s/%s", runtime.GOOS, runtime.GOARCH)
	}
	name := strings.TrimSuffix(sf19Assets[i], ".tar.gz") + ".zip"
	download := "https://github.com/official-stockfish/Stockfish/releases/download/sf_test/" + name

	for _, tc := range []struct {
		name, digest string
		ok           bool
	}{{"matching", good, true}, {"mismatched", "sha256:" + strings.Repeat("0", 64), false}, {"missing", "", false}} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			cwd, _ := os.Getwd()
			if err := os.Chdir(dir); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { os.Chdir(cwd) })

			release, _ := json.Marshal(map[string]any{"assets": []map[string]string{{"name": name, "browser_download_url": download, "digest": tc.digest}}})
			original := apiClient
			apiClient = &http.Client{Transport: fakeGitHub{
				"https://api.github.com/repos/official-stockfish/Stockfish/releases/latest": release,
				download: archive.Bytes(),
			}}
			t.Cleanup(func() { apiClient = original })

			path, err := installEngine(context.Background())
			if tc.ok {
				if err != nil {
					t.Fatal(err)
				}
				if _, err := os.Stat(filepath.Join(dir, path)); err != nil {
					t.Fatal("engine not installed:", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), "checksum") {
				t.Fatalf("want checksum error, got %v", err)
			}
			if entries, _ := os.ReadDir(filepath.Join(dir, "engines")); len(entries) != 0 {
				t.Fatalf("files left after a failed check: %v", entries)
			}
		})
	}
}

// slowGitHub serves canned bodies like fakeGitHub, but the body of slow waits
// for delay before its first byte and gives up when the request is cancelled.
type slowGitHub struct {
	bodies map[string][]byte
	slow   string
	delay  time.Duration
}

func (f slowGitHub) RoundTrip(r *http.Request) (*http.Response, error) {
	res, _ := fakeGitHub(f.bodies).RoundTrip(r)
	if r.URL.String() == f.slow {
		res.Body = &slowBody{Reader: bytes.NewReader(f.bodies[f.slow]), ctx: r.Context(), delay: f.delay}
	}
	return res, nil
}

type slowBody struct {
	*bytes.Reader
	ctx     context.Context
	delay   time.Duration
	started bool
}

func (b *slowBody) Read(p []byte) (int, error) {
	if !b.started {
		b.started = true
		select {
		case <-time.After(b.delay):
		case <-b.ctx.Done():
			return 0, b.ctx.Err()
		}
	}
	return b.Reader.Read(p)
}
func (b *slowBody) Close() error { return nil }

func TestInstallEngineSlowDownload(t *testing.T) {
	var archive bytes.Buffer
	z := zip.NewWriter(&archive)
	w, _ := z.Create("stockfish/stockfish")
	w.Write([]byte("fake engine"))
	z.Close()
	sum := sha256.Sum256(archive.Bytes())

	i := stockfishAsset(sf19Assets, runtime.GOOS, runtime.GOARCH)
	if i < 0 {
		t.Skipf("no sf_19 build for %s/%s", runtime.GOOS, runtime.GOARCH)
	}
	name := strings.TrimSuffix(sf19Assets[i], ".tar.gz") + ".zip"
	download := "https://github.com/official-stockfish/Stockfish/releases/download/sf_test/" + name
	release, _ := json.Marshal(map[string]any{"assets": []map[string]string{{"name": name, "browser_download_url": download, "digest": "sha256:" + hex.EncodeToString(sum[:])}}})
	bodies := map[string][]byte{"https://api.github.com/repos/official-stockfish/Stockfish/releases/latest": release, download: archive.Bytes()}

	for _, tc := range []struct {
		name   string
		delay  time.Duration
		cancel time.Duration
		ok     bool
	}{
		// The download outlasts the client timeout but finishes within the context.
		{"slower than client timeout", 300 * time.Millisecond, 0, true},
		// A cancelled context stops a download that would otherwise hang.
		{"cancelled", time.Minute, time.Second, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			cwd, _ := os.Getwd()
			if err := os.Chdir(dir); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { os.Chdir(cwd) })
			original := apiClient
			apiClient = &http.Client{Timeout: 100 * time.Millisecond, Transport: slowGitHub{bodies, download, tc.delay}}
			t.Cleanup(func() { apiClient = original })

			ctx := context.Background()
			if tc.cancel > 0 {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, tc.cancel)
				defer cancel()
			}
			start := time.Now()
			_, err := installEngine(ctx)
			if tc.ok && err != nil {
				t.Fatal(err)
			}
			if !tc.ok {
				if !errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("want a cancellation error, got %v", err)
				}
				if time.Since(start) > 10*time.Second {
					t.Fatal("cancelled download did not stop promptly")
				}
			}
		})
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestInstallEngineReleaseErrorsNameGitHub(t *testing.T) {
	for _, tc := range []struct {
		name string
		rt   roundTripFunc
		want string
	}{
		{"rate limited", func(r *http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusForbidden, Body: http.NoBody, Header: http.Header{}, Request: r}, nil
		}, "cannot fetch official Stockfish release from GitHub: server returned 403"},
		{"unreachable", func(r *http.Request) (*http.Response, error) {
			return nil, errors.New("dial failed")
		}, "cannot fetch official Stockfish release from GitHub: request failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			original := apiClient
			apiClient = &http.Client{Transport: tc.rt}
			t.Cleanup(func() { apiClient = original })
			_, err := installEngine(context.Background())
			if err == nil || !strings.HasPrefix(err.Error(), tc.want) || strings.Contains(err.Error(), "Chess.com") {
				t.Fatalf("want %q, got %v", tc.want, err)
			}
		})
	}
}

// fakeRelease returns a release whose only asset is this platform's build
// with extension ext, and that asset's download URL.
func fakeRelease(t *testing.T, ext, digest string) (release []byte, download string) {
	t.Helper()
	i := stockfishAsset(sf19Assets, runtime.GOOS, runtime.GOARCH)
	if i < 0 {
		t.Skipf("no sf_19 build for %s/%s", runtime.GOOS, runtime.GOARCH)
	}
	name := strings.TrimSuffix(strings.TrimSuffix(sf19Assets[i], ".tar.gz"), ".zip") + ext
	download = "https://github.com/official-stockfish/Stockfish/releases/download/sf_test/" + name
	release, _ = json.Marshal(map[string]any{"assets": []map[string]string{{"name": name, "browser_download_url": download, "digest": digest}}})
	return release, download
}

// inTempDir runs the rest of the test from root/a/b, so archive entries that
// climb two levels still land inside root, and returns root.
func inTempDir(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, "a", "b")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	cwd, _ := os.Getwd()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chdir(cwd) })
	return root
}

// useTransport points apiClient at rt for the rest of the test.
func useTransport(t *testing.T, rt http.RoundTripper) {
	original := apiClient
	apiClient = &http.Client{Transport: rt}
	t.Cleanup(func() { apiClient = original })
}

// installArchive installs archive as a verified release asset with extension ext.
func installArchive(t *testing.T, ext string, archive []byte, notify ...func(int)) (string, error) {
	t.Helper()
	sum := sha256.Sum256(archive)
	release, download := fakeRelease(t, ext, "sha256:"+hex.EncodeToString(sum[:]))
	useTransport(t, fakeGitHub{"https://api.github.com/repos/official-stockfish/Stockfish/releases/latest": release, download: archive})
	return installEngine(context.Background(), notify...)
}

// zipArchive and tarGzArchive take name, body pairs; a name ending in / is a directory.
func zipArchive(entries ...string) []byte {
	var b bytes.Buffer
	z := zip.NewWriter(&b)
	for i := 0; i < len(entries); i += 2 {
		w, _ := z.Create(entries[i])
		w.Write([]byte(entries[i+1]))
	}
	z.Close()
	return b.Bytes()
}

func tarGzArchive(entries ...string) []byte {
	var b bytes.Buffer
	gz := gzip.NewWriter(&b)
	tw := tar.NewWriter(gz)
	for i := 0; i < len(entries); i += 2 {
		h := &tar.Header{Name: entries[i], Mode: 0700, Size: int64(len(entries[i+1])), Typeflag: tar.TypeReg}
		if strings.HasSuffix(h.Name, "/") {
			h.Typeflag, h.Size = tar.TypeDir, 0
		}
		tw.WriteHeader(h)
		tw.Write([]byte(entries[i+1]))
	}
	tw.Close()
	gz.Close()
	return b.Bytes()
}

func TestInstallEngineExtractsArchives(t *testing.T) {
	engine := "a/b/engines/stockfish-local"
	if runtime.GOOS == "windows" {
		engine += ".exe"
	}
	license := "a/b/engines/Stockfish-LICENSE.txt"
	release := []string{"stockfish/", "", "stockfish/stockfish-test", "engine", "stockfish/Copying.txt", "license", "stockfish/README.md", "readme"}
	traversal := []string{"../../evil", "evil", "../../stockfish", "engine"}
	missing := []string{"stockfish/", "", "stockfish/README.md", "readme"}
	for _, format := range []struct {
		ext   string
		build func(...string) []byte
	}{{".zip", zipArchive}, {".tar.gz", tarGzArchive}} {
		for _, tc := range []struct {
			name    string
			archive []byte
			files   []string // every file left under the test root
			err     string
		}{
			{"release layout", format.build(release...), []string{license, engine}, ""},
			{"path traversal", format.build(traversal...), []string{engine}, ""},
			{"missing executable", format.build(missing...), nil, "executable not found"},
			{"not an archive", []byte("not an archive"), nil, "valid"},
		} {
			t.Run(format.ext+"/"+tc.name, func(t *testing.T) {
				root := inTempDir(t)
				path, err := installArchive(t, format.ext, tc.archive)
				if tc.err == "" && err != nil {
					t.Fatal(err)
				}
				if tc.err != "" && (err == nil || !strings.Contains(err.Error(), tc.err)) {
					t.Fatalf("want error containing %q, got %v", tc.err, err)
				}
				var files []string
				filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
					if err == nil && !d.IsDir() {
						rel, _ := filepath.Rel(root, p)
						files = append(files, filepath.ToSlash(rel))
					}
					return err
				})
				if strings.Join(files, " ") != strings.Join(tc.files, " ") {
					t.Fatalf("files %v, want %v", files, tc.files)
				}
				if err == nil {
					if body, _ := os.ReadFile(path); string(body) != "engine" {
						t.Fatalf("engine contains %q", body)
					}
				}
			})
		}
	}
}

func TestInstallEngineReportsProgress(t *testing.T) {
	inTempDir(t)
	var got []int
	if _, err := installArchive(t, ".zip", zipArchive("stockfish/stockfish-test", "engine"), func(p int) { got = append(got, p) }); err != nil {
		t.Fatal(err)
	}
	for i := 1; i < len(got); i++ {
		if got[i] < got[i-1] {
			t.Fatalf("progress went backwards: %v", got)
		}
	}
	if len(got) == 0 || got[len(got)-1] != 95 {
		t.Fatalf("progress %v, want it to end at 95", got)
	}
}

// zeros is an endless stream of zero bytes.
type zeros struct{}

func (zeros) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}

func TestInstallEngineDownloadErrors(t *testing.T) {
	for _, tc := range []struct {
		name     string
		download roundTripFunc
		want     string
	}{
		{"oversized", func(r *http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(zeros{}), ContentLength: -1, Header: http.Header{}, Request: r}, nil
		}, "too large"},
		{"not found", func(r *http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusNotFound, Body: http.NoBody, Header: http.Header{}, Request: r}, nil
		}, "returned 404"},
		{"unreachable", func(r *http.Request) (*http.Response, error) {
			return nil, errors.New("dial failed")
		}, "download failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := inTempDir(t)
			release, download := fakeRelease(t, ".zip", "sha256:"+strings.Repeat("0", 64))
			useTransport(t, roundTripFunc(func(r *http.Request) (*http.Response, error) {
				if r.URL.String() == download {
					return tc.download(r)
				}
				return fakeGitHub{"https://api.github.com/repos/official-stockfish/Stockfish/releases/latest": release}.RoundTrip(r)
			}))
			_, err := installEngine(context.Background())
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want error containing %q, got %v", tc.want, err)
			}
			if entries, _ := os.ReadDir(filepath.Join(root, "a", "b", "engines")); len(entries) != 0 {
				t.Fatalf("files left after a failed download: %v", entries)
			}
		})
	}
}
