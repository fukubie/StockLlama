package main

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
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
