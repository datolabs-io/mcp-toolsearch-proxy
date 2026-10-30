package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// One server is kept per distinct text of upstream instructions, and no
// more than maxServers of them, however many texts callers bring.
func TestPerCallerKeepsAtMostMaxServers(t *testing.T) {
	t.Parallel()
	p := &perCaller{o: (*Options)(nil).withDefaults(), servers: map[string]*mcp.Server{}}
	request := func(upstream string) *http.Request {
		r := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/mcp", nil)
		return r.WithContext(context.WithValue(r.Context(), upstreamKey{}, upstream))
	}

	first := p.server(request("0"))
	require.Same(t, first, p.server(request("0")), "a text's server is kept")
	for i := range 3 * maxServers {
		srv := p.server(request(strconv.Itoa(i)))
		assert.Same(t, srv, p.server(request(strconv.Itoa(i))))
		assert.LessOrEqual(t, len(p.servers), maxServers)
	}
	assert.NotSame(t, first, p.server(request("0")), "dropped to make room, then made again")
}
