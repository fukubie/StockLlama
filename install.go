package main

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

type progressReader struct {
	reader      io.Reader
	total, read int64
	notify      func(int)
}

func (p *progressReader) Read(b []byte) (int, error) {
	n, err := p.reader.Read(b)
	p.read += int64(n)
	if p.total > 0 && p.notify != nil {
		p.notify(int(p.read * 95 / p.total))
	}
	return n, err
}

// stockfishAsset returns the index of the release asset to download for goos
// and goarch, or -1 if none fits. macOS ships one universal build for both
// architectures; other systems ship one universal build per architecture,
// so an architecture with no official build matches nothing.
func stockfishAsset(names []string, goos, goarch string) int {
	arch := map[string]string{"amd64": "x86-64", "arm64": "arm64", "riscv64": "riscv64"}[goarch]
	if arch == "" && goos != "darwin" {
		return -1
	}
	for i, n := range names {
		if goos == "darwin" {
			if strings.HasPrefix(n, "stockfish-macos-universal") {
				return i
			}
		} else if strings.Contains(n, goos+"-"+arch) && strings.Contains(n, "universal") {
			return i
		}
	}
	return -1
}

func installEngine(ctx context.Context, notify ...func(int)) (string, error) {
	var release struct {
		Assets []struct {
			Name   string `json:"name"`
			URL    string `json:"browser_download_url"`
			Digest string `json:"digest"`
		} `json:"assets"`
	}
	if err := getJSON(ctx, apiClient, "https://api.github.com/repos/official-stockfish/Stockfish/releases/latest", &release); err != nil {
		return "", fmt.Errorf("cannot fetch official Stockfish release: %w", err)
	}
	names := make([]string, len(release.Assets))
	for i, a := range release.Assets {
		names[i] = a.Name
	}
	url, name, digest := "", "", ""
	if i := stockfishAsset(names, runtime.GOOS, runtime.GOARCH); i >= 0 {
		a := release.Assets[i]
		url, name, digest = a.URL, a.Name, a.Digest
	}
	if url == "" {
		return "", fmt.Errorf("no universal Stockfish build found; download from stockfishchess.org and set its path")
	}
	if !strings.HasPrefix(url, "https://github.com/official-stockfish/Stockfish/releases/download/") {
		return "", fmt.Errorf("unexpected Stockfish download URL")
	}
	req, _ := http.NewRequestWithContext(ctx, "GET", url, nil)
	// The archive is large, so the download has no overall client timeout;
	// the caller's context bounds it instead.
	download := &http.Client{Transport: apiClient.Transport, CheckRedirect: apiClient.CheckRedirect}
	res, err := download.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return "", fmt.Errorf("engine download cancelled or timed out: %w", ctx.Err())
		}
		return "", fmt.Errorf("Stockfish download failed")
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		return "", fmt.Errorf("Stockfish download returned %d", res.StatusCode)
	}
	os.MkdirAll("engines", 0700)
	file, err := os.CreateTemp("engines", "download-*")
	if err != nil {
		return "", err
	}
	defer os.Remove(file.Name())
	var reader io.Reader = res.Body
	if len(notify) > 0 {
		reader = &progressReader{reader: res.Body, total: res.ContentLength, notify: notify[0]}
	}
	hash := sha256.New()
	n, err := io.Copy(io.MultiWriter(file, hash), io.LimitReader(reader, 200<<20))
	file.Close()
	if err != nil && ctx.Err() != nil {
		return "", fmt.Errorf("engine download cancelled or timed out: %w", ctx.Err())
	}
	if err != nil || n >= 200<<20 {
		return "", fmt.Errorf("engine download incomplete or too large")
	}
	if digest == "" || digest != "sha256:"+hex.EncodeToString(hash.Sum(nil)) {
		return "", fmt.Errorf("engine download failed checksum verification")
	}
	destination := filepath.Join("engines", "stockfish-local")
	if runtime.GOOS == "windows" {
		destination += ".exe"
	}
	// Only extract the engine and its license; do not trust paths from archives.
	write := func(name string, r io.Reader) error {
		base := filepath.Base(name)
		target := ""
		if strings.HasPrefix(base, "stockfish") && (strings.HasSuffix(base, ".exe") || !strings.Contains(base, ".")) {
			target = destination
		}
		if strings.EqualFold(base, "Copying.txt") {
			target = filepath.Join("engines", "Stockfish-LICENSE.txt")
		}
		if target == "" {
			return nil
		}
		f, e := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0700)
		if e != nil {
			return e
		}
		defer f.Close()
		_, e = io.Copy(f, io.LimitReader(r, 150<<20))
		return e
	}
	if strings.HasSuffix(name, ".zip") {
		z, e := zip.OpenReader(file.Name())
		if e != nil {
			return "", e
		}
		defer z.Close()
		for _, entry := range z.File {
			if entry.FileInfo().IsDir() {
				continue
			}
			r, e := entry.Open()
			if e != nil {
				return "", e
			}
			e = write(entry.Name, r)
			r.Close()
			if e != nil {
				return "", e
			}
		}
	} else {
		f, e := os.Open(file.Name())
		if e != nil {
			return "", e
		}
		defer f.Close()
		gz, e := gzip.NewReader(f)
		if e != nil {
			return "", e
		}
		defer gz.Close()
		tr := tar.NewReader(gz)
		for {
			h, e := tr.Next()
			if e == io.EOF {
				break
			}
			if e != nil {
				return "", e
			}
			if h.Typeflag == tar.TypeReg {
				if e = write(h.Name, tr); e != nil {
					return "", e
				}
			}
		}
	}
	if _, err = os.Stat(destination); err != nil {
		return "", fmt.Errorf("engine executable not found in download")
	}
	return destination, nil
}
