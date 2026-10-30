package bench

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"time"
)

// How long the proxy gets to answer its health check, and how often it is
// asked.
const (
	proxyStartTimeout = 30 * time.Second
	healthInterval    = 100 * time.Millisecond
)

// proxyProc is a running mcp-toolsearch-proxy.
type proxyProc struct {
	URL string

	cmd *exec.Cmd
}

// buildProxy builds the repository's proxy into bin.
func buildProxy(ctx context.Context, bin string) error {
	args := []string{"build", "-o", bin, "./cmd/mcp-toolsearch-proxy"}
	cmd := exec.CommandContext(ctx, "go", args...) //nolint:gosec // our own build
	cmd.Dir, cmd.Stdout, cmd.Stderr = repoRoot, os.Stderr, os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("build the proxy: %w", err)
	}
	return nil
}

// startProxy runs bin on a free loopback port in front of upstreamURL, with
// flags added to its defaults, and waits until it is healthy.
func startProxy(ctx context.Context, bin, upstreamURL string, flags ...string) (*proxyProc, error) {
	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("find a port: %w", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	args := append([]string{"-addr", addr, "-upstream", upstreamURL, "-log-level", "warn"}, flags...)
	cmd := exec.CommandContext(context.WithoutCancel(ctx), bin, args...) //nolint:gosec // the binary just built
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	if err = cmd.Start(); err != nil {
		return nil, fmt.Errorf("start the proxy: %w", err)
	}
	p := &proxyProc{URL: "http://" + addr + "/mcp", cmd: cmd}
	if err = p.wait(ctx, "http://"+addr+"/healthz"); err != nil {
		p.Close()
		return nil, err
	}
	return p, nil
}

// Close stops the proxy.
func (p *proxyProc) Close() {
	_ = p.cmd.Process.Kill()
	_ = p.cmd.Wait()
}

func (p *proxyProc) wait(ctx context.Context, health string) error {
	ctx, cancel := context.WithTimeout(ctx, proxyStartTimeout)
	defer cancel()
	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, health, nil)
		if err != nil {
			return fmt.Errorf("health check: %w", err)
		}
		if resp, derr := http.DefaultClient.Do(req); derr == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return errors.New("the proxy did not become healthy")
		case <-time.After(healthInterval):
		}
	}
}
