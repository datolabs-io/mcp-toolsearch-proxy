package embed

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
)

// publicFile is the mode of a fetched file: the model is public, and
// readable by all, so that whoever runs the proxy can load what another
// user fetched.
const publicFile = 0o644

// Source is a published static model: where its files are and what they
// must hash to.
type Source struct {
	// Name identifies the model, such as "potion-multilingual-128M".
	Name string
	// BaseURL is the directory the files are fetched from.
	BaseURL string
	// SHA256 maps each file to its hex digest.
	SHA256 map[string]string
}

// DefaultSource is the model the proxy is built and evaluated with:
// potion-multilingual-128M by Minish (MIT), distilled from BGE-M3, which
// covers 101 languages. Its revision is pinned, so a fetch always gives the
// same files.
//
//nolint:gochecknoglobals // a constant description of published files
var DefaultSource = Source{
	Name:    "potion-multilingual-128M",
	BaseURL: "https://huggingface.co/minishlab/potion-multilingual-128M/resolve/73908c3438cf03b6a01bcb9611d62b23d0726f08/",
	SHA256: map[string]string{ //nolint:gosec // file digests, not credentials
		TokenizerFile: "19f1909063da3cfe3bd83a782381f040dccea475f4816de11116444a73e1b6a1",
		WeightsFile:   "14b5eb39cb4ce5666da8ad1f3dc6be4346e9b2d601c073302fa0a31bf7943397",
	},
}

// Fetch downloads src's files into dir, which it creates, skipping a file
// already there with the right digest. A file is written under a temporary
// name and renamed once its digest matches, so dir never holds a partial or
// altered file under the real name.
func Fetch(ctx context.Context, client *http.Client, src Source, dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil { //nolint:gosec // public files
		return fmt.Errorf("embed: %w", err)
	}
	for _, name := range []string{TokenizerFile, WeightsFile} {
		want, ok := src.SHA256[name]
		if !ok {
			return fmt.Errorf("embed: no digest for %s", name)
		}
		path := filepath.Join(dir, name)
		if got, err := digest(path); err == nil && got == want {
			continue
		}
		if err := fetchFile(ctx, client, src.BaseURL+name, path, want); err != nil {
			return err
		}
	}
	return nil
}

// Verify reports whether dir holds src's files with the right digests.
func Verify(src Source, dir string) error {
	for name, want := range src.SHA256 {
		got, err := digest(filepath.Join(dir, name))
		if err != nil {
			return fmt.Errorf("embed: %w", err)
		}
		if got != want {
			return fmt.Errorf("embed: %s has digest %s; want %s", name, got, want)
		}
	}
	return nil
}

func fetchFile(ctx context.Context, client *http.Client, url, path, want string) (err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("embed: %w", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("embed: fetch %s: %w", filepath.Base(path), err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("embed: fetch %s: %s", filepath.Base(path), resp.Status)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return fmt.Errorf("embed: %w", err)
	}
	defer func() {
		if err != nil {
			_ = os.Remove(tmp.Name())
		}
	}()
	if err = tmp.Chmod(publicFile); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("embed: %w", err)
	}
	h := sha256.New()
	_, err = io.Copy(io.MultiWriter(tmp, h), resp.Body)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("embed: fetch %s: %w", filepath.Base(path), err)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != want {
		return fmt.Errorf("embed: %s has digest %s; want %s", filepath.Base(path), got, want)
	}
	if err = os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("embed: %w", err)
	}
	return nil
}

func digest(path string) (string, error) {
	f, err := os.Open(path) //nolint:gosec // a path the operator chose
	if err != nil {
		return "", err //nolint:wrapcheck // wrapped by the callers
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err = io.Copy(h, f); err != nil {
		return "", err //nolint:wrapcheck // wrapped by the callers
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
