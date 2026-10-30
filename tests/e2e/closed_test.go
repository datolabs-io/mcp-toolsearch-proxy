//go:build e2e

package e2e_test

import (
	"bufio"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// metric returns the value of the sample named series, labels included, at
// the Prometheus endpoint url, or 0 when there is none.
func metric(t *testing.T, url, series string) float64 {
	t.Helper()
	//nolint:gosec // the stack's own endpoint, from the test's environment
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, nil)
	require.NoError(t, err)
	resp, err := http.DefaultClient.Do(req) //nolint:gosec // as above
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	sc := bufio.NewScanner(io.LimitReader(resp.Body, 1<<20))
	for sc.Scan() {
		if value, ok := strings.CutPrefix(sc.Text(), series+" "); ok {
			v, perr := strconv.ParseFloat(value, 64)
			require.NoError(t, perr)
			return v
		}
	}
	require.NoError(t, sc.Err())
	return 0
}

// One server behind a failClosed gateway that stops listing its tools fails
// the gateway's whole listing. The proxy keeps serving the catalog it built
// before: its catalogs are stale at once, and listing them again in the
// background fails.
//
// The server answers only the stack's first listing on that route, so this
// test passes once per stack, or again within the second proxy's catalog
// max age (an hour).
func TestAServerThatStopsListingLeavesTheCatalogServed(t *testing.T) {
	t.Parallel()
	url, all := os.Getenv("E2E_CLOSED_URL"), os.Getenv("E2E_CLOSED_ALL_URL")
	metrics := os.Getenv("E2E_CLOSED_METRICS_URL")
	if url == "" || all == "" || metrics == "" {
		t.Skip("E2E_CLOSED_URL, E2E_CLOSED_ALL_URL and E2E_CLOSED_METRICS_URL are not set")
	}
	const failed = `mcp_toolsearch_catalog_builds_total{outcome="error"}`
	tok := token(t, "alice")
	cs := connect(t, url, tok)

	first := search(t, cs, "invoice", 5)
	_, err := connect(t, all, tok).ListTools(t.Context(), nil)
	// The search lists again in the background, and that fails.
	search(t, cs, "invoice", 5)
	require.Eventually(t, func() bool { return metric(t, metrics, failed) >= 1 }, time.Minute, 100*time.Millisecond,
		"the proxy never listed again")
	after := search(t, cs, "invoice", 5)

	assert.Contains(t, first.names(), "billing_get_invoice")
	assert.Equal(t, 3, first.Total, "the weather tools and the invoice")
	require.Error(t, err, "failClosed: the gateway fails the listing of every server")
	assert.Equal(t, first.Total, after.Total, "the catalog built before is still served")
	assert.Contains(t, after.names(), "billing_get_invoice")
	assert.GreaterOrEqual(t, metric(t, metrics, "mcp_toolsearch_catalog_stale_served_total"), float64(2))
}
