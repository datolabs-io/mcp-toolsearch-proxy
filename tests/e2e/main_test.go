//go:build e2e

package e2e_test

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// defaultWait is how long the tests wait for the stack to answer.
const defaultWait = 2 * time.Minute

// TestMain waits for the stack, which may still be starting, to answer an
// authenticated request through the proxy.
func TestMain(m *testing.M) {
	if err := waitForStack(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(m.Run())
}

func waitForStack() error {
	url := os.Getenv("E2E_SEARCH_URL")
	if url == "" {
		return nil // each test reports the missing setting
	}
	wait := defaultWait
	if s := os.Getenv("E2E_WAIT"); s != "" {
		d, err := time.ParseDuration(s)
		if err != nil {
			return fmt.Errorf("E2E_WAIT: %w", err)
		}
		wait = d
	}
	tok := os.Getenv("E2E_TOKEN_ALICE")
	if tok == "" {
		raw, err := os.ReadFile("testdata/alice.jwt")
		if err != nil {
			return fmt.Errorf("read token: %w", err)
		}
		tok = strings.TrimSpace(string(raw))
	}
	ctx, cancel := context.WithTimeout(context.Background(), wait)
	defer cancel()
	var last error
	for ctx.Err() == nil {
		client := mcp.NewClient(&mcp.Implementation{Name: "e2e-wait", Version: "0"}, nil)
		cs, err := client.Connect(ctx, &mcp.StreamableClientTransport{
			Endpoint: url, HTTPClient: &http.Client{Transport: bearer{tok}}, DisableStandaloneSSE: true, MaxRetries: -1,
		}, nil)
		if err == nil {
			_, err = cs.ListTools(ctx, nil)
			_ = cs.Close()
			if err == nil {
				return nil
			}
		}
		last = err
		select {
		case <-ctx.Done():
		case <-time.After(time.Second):
		}
	}
	return fmt.Errorf("the stack at %s did not answer within %s: %w", url, wait, last)
}
