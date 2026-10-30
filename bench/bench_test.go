package bench

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/anthropics/anthropic-sdk-go/v2"
	"github.com/stretchr/testify/require"
)

// Settings of the benchmarks, from the environment. The prices are Claude
// Opus 5.5's, in dollars per million tokens, cache writes at the
// five-minute rate; set BENCH_PRICES for another model.
const (
	envContextSizes = "BENCH_SIZES"    // catalog sizes, comma-separated, 50 to 1490
	envAgent        = "BENCH_AGENT"    // 1 runs BenchmarkAgent, whose requests are billed
	envModel        = "BENCH_MODEL"    // the model that counts tokens or runs the tasks
	envTasks        = "BENCH_TASKS"    // how many of the evaluation set's questions BenchmarkAgent runs
	envModes        = "BENCH_MODES"    // all, toolsearch and proxy, comma-separated
	envParallel     = "BENCH_PARALLEL" // how many runs at once
	envEffort       = "BENCH_EFFORT"   // output effort
	envMaxTurns     = "BENCH_MAX_TURNS"
	envPrices       = "BENCH_PRICES" // input, output, cache read, cache write

	modelDir = "../.cache/model"
)

// bench is what both benchmarks share: the proxy, built once, the
// evaluation set and the distractor domains.
type bench struct {
	contextSetup

	tasks []Task
}

//nolint:gochecknoglobals // the proxy is built once for every benchmark
var (
	setupOnce sync.Once
	errSetup  error
)

// Where the proxy is built, kept between runs, and where BenchmarkAgent
// appends each run's result, a JSON object per line.
const (
	proxyBin      = "../.cache/bench/mcp-toolsearch-proxy"
	runsFile      = "../.cache/bench/agent.jsonl"
	resultsAccess = 0o600
)

func setup(b *testing.B) bench {
	b.Helper()
	bin, err := filepath.Abs(proxyBin)
	require.NoError(b, err)
	require.NoError(b, os.MkdirAll(filepath.Dir(bin), 0o750))
	setupOnce.Do(func() { errSetup = buildProxy(b.Context(), bin) })
	require.NoError(b, errSetup)
	core, tasks, err := loadEval()
	require.NoError(b, err)
	domains, err := loadDomains()
	require.NoError(b, err)
	return bench{contextSetup{bin: bin, flags: embedderFlags(b), core: core, domains: domains}, tasks}
}

// embedderFlags are the proxy's flags for the built-in model when it has
// been fetched, else for lexical search.
func embedderFlags(b *testing.B) []string {
	b.Helper()
	dir, err := filepath.Abs(modelDir)
	require.NoError(b, err)
	if _, err = os.Stat(filepath.Join(dir, "model.safetensors")); err != nil {
		b.Log("no built-in model in " + modelDir + " (mise run model:fetch): the proxy searches lexically only")
		return []string{"-embedder", "none"}
	}
	return []string{"-embedder", "builtin", "-model-dir", dir}
}

func setting(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

func intSetting(b *testing.B, name string, def int) int {
	b.Helper()
	n, err := strconv.Atoi(setting(name, strconv.Itoa(def)))
	require.NoError(b, err, name)
	return n
}

func sizes(b *testing.B, def string) []int {
	b.Helper()
	var out []int
	for f := range strings.SplitSeq(setting(envContextSizes, def), ",") {
		n, err := strconv.Atoi(strings.TrimSpace(f))
		require.NoError(b, err, envContextSizes)
		out = append(out, n)
	}
	return out
}

func apiKey() bool {
	return os.Getenv("ANTHROPIC_API_KEY") != "" || os.Getenv("ANTHROPIC_AUTH_TOKEN") != ""
}

// BenchmarkContext reports, per catalog size and mode, what a client loads
// before the model does anything, and what one proxy search adds. The
// measurement is taken once; its ns/op is not reported.
func BenchmarkContext(b *testing.B) {
	s := setup(b)
	s.contextSetup.tasks = selectTasks(s.tasks, intSetting(b, "BENCH_SEARCHES", 20))
	var c counter
	if apiKey() {
		c = apiCounter{client: anthropic.NewClient(), model: setting(envModel, "claude-opus-5-5")}
	} else {
		b.Log("no ANTHROPIC_API_KEY: bytes only, no tokens")
	}
	for _, n := range sizes(b, "50,250,500,1000,1490") {
		row, err := measureSize(b.Context(), s.contextSetup, n, c)
		require.NoError(b, err)
		for _, m := range []struct {
			name string
			sz   size
		}{{"all", row.Direct}, {"toolsearch", row.ToolSearch}, {"proxy", row.Proxy}, {"proxy-search", row.Search}} {
			sz := m.sz
			b.Run(fmt.Sprintf("tools=%d/mode=%s", n, m.name), func(b *testing.B) {
				for b.Loop() { // measured above, once
				}
				b.ReportMetric(0, "ns/op")
				if sz.Bytes >= 0 {
					b.ReportMetric(float64(sz.Bytes), "bytes/listing")
				}
				if sz.Tokens >= 0 {
					b.ReportMetric(float64(sz.Tokens), "tokens/listing")
				}
			})
		}
	}
}

// BenchmarkAgent runs the tasks once per op, per catalog size and mode,
// and reports per task the share that called the right tool, the turns,
// tokens and dollars it took. Run it with -benchtime=1x and -count for
// repeats, and compare the modes with benchstat -col /mode.
func BenchmarkAgent(b *testing.B) {
	if os.Getenv(envAgent) != "1" {
		b.Skip("set " + envAgent + "=1 and ANTHROPIC_API_KEY to run it: its requests are billed")
	}
	if !apiKey() {
		b.Fatal(envAgent + " is set but ANTHROPIC_API_KEY is not")
	}
	s := setup(b)
	tasks := selectTasks(s.tasks, intSetting(b, envTasks, 30))
	a := agentSetup{
		client:   anthropic.NewClient(),
		model:    setting(envModel, "claude-opus-5-5"),
		effort:   anthropic.OutputConfigEffort(setting(envEffort, "medium")),
		maxTurns: intSetting(b, envMaxTurns, 8),
		prices:   parsePrices(b, setting(envPrices, "4,20,0.2,5")),
	}
	parallel := intSetting(b, envParallel, 4)
	runs, err := os.OpenFile(runsFile, os.O_CREATE|os.O_APPEND|os.O_WRONLY, resultsAccess)
	require.NoError(b, err)
	b.Cleanup(func() { _ = runs.Close() })
	enc := json.NewEncoder(runs)
	var modes []mode
	for f := range strings.SplitSeq(setting(envModes, "all,toolsearch,proxy"), ",") {
		m, merr := parseMode(strings.TrimSpace(f))
		require.NoError(b, merr)
		modes = append(modes, m)
	}

	for _, n := range sizes(b, "500") {
		tools, cerr := catalog(s.core, s.domains, n)
		require.NoError(b, cerr)
		e, serr := startEnv(b.Context(), tools, s.bin, s.flags...)
		require.NoError(b, serr)
		for _, m := range modes {
			b.Run(fmt.Sprintf("tools=%d/mode=%s", n, m), func(b *testing.B) {
				var all []runResult
				for b.Loop() {
					all = append(all, runAll(b.Context(), a, e, []mode{m}, tasks, 1, parallel, func(r runResult) {
						if r.Err != "" {
							b.Logf("%s: %s", r.Query, r.Err)
						}
						// done runs in a worker holding a lock: report, do not stop.
						if eerr := enc.Encode(r); eerr != nil {
							b.Error(eerr)
						}
					})...)
				}
				report(b, all)
			})
		}
		e.Close()
	}
}

// report reports results per task.
func report(b *testing.B, results []runResult) {
	b.Helper()
	var wins, errs int
	var turns, in, out, cost float64
	for _, r := range results {
		if r.Stop == stopError {
			errs++
		}
		if r.Success {
			wins++
		}
		turns += float64(r.Turns)
		in += float64(r.Usage.Input + r.Usage.CacheRead + r.Usage.CacheWrite)
		out += float64(r.Usage.Output)
		cost += r.Cost
	}
	n := float64(max(1, len(results)))
	b.ReportMetric(float64(wins)/n, "success/task")
	b.ReportMetric(float64(errs)/n, "errors/task")
	b.ReportMetric(turns/n, "turns/task")
	b.ReportMetric(in/n, "in-tokens/task")
	b.ReportMetric(out/n, "out-tokens/task")
	b.ReportMetric(cost/n, "usd/task")
	if wins > 0 {
		b.ReportMetric(cost/float64(wins), "usd/success")
	}
}

func parsePrices(b *testing.B, s string) prices {
	b.Helper()
	var p [4]float64
	fields := strings.Split(s, ",")
	require.Len(b, fields, len(p), envPrices+": input, output, cache read, cache write")
	for i, f := range fields {
		v, err := strconv.ParseFloat(strings.TrimSpace(f), 64)
		require.NoError(b, err, envPrices)
		p[i] = v
	}
	return prices{In: p[0], Out: p[1], CacheRead: p[2], CacheWrite: p[3]}
}
