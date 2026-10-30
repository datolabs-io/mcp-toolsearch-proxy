package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// configDoc is the configuration reference, generated from the flags.
const configDoc = "../../docs/configuration.md"

// TestConfigurationReference keeps docs/configuration.md in step with the
// flags. `mise run docs:config` regenerates it.
func TestConfigurationReference(t *testing.T) {
	t.Parallel()
	want := configurationReference(t)

	if os.Getenv("UPDATE_CONFIG_DOC") != "" {
		require.NoError(t, os.WriteFile(configDoc, []byte(want), 0o644)) //nolint:gosec // a document
	}
	got, err := os.ReadFile(configDoc)

	require.NoError(t, err)
	assert.Equal(t, want, string(got), "docs/configuration.md is out of date: run `mise run docs:config`")
}

// configurationReference renders every flag, with its environment variable
// and default, as docs/configuration.md.
func configurationReference(t *testing.T) string {
	t.Helper()
	var fs *flag.FlagSet
	_, _ = parseConfigWith([]string{"-h"}, envOf(nil), io.Discard, func(f *flag.FlagSet) { fs = f })
	require.NotNil(t, fs)
	// The environment variable a flag's usage ends with.
	envName := regexp.MustCompile(`\s*\(([A-Z_]+)\)$`)
	// URLs, which Markdown wants in angle brackets.
	link := regexp.MustCompile(`https?://[^\s,;)]+`)

	var b strings.Builder
	b.WriteString(`# Configuration

<!-- Generated from the flags by ` + "`mise run docs:config`" + `; do not edit by hand. -->

Every setting is a flag with an environment variable as its default; a flag on the command line wins. The proxy takes
no secret. ` + "`mcp-toolsearch-proxy -h`" + ` prints the same list.

| Flag | Environment variable | Default | Meaning |
| --- | --- | --- | --- |
`)
	fs.VisitAll(func(f *flag.Flag) {
		usage, env := f.Usage, ""
		if m := envName.FindStringSubmatch(usage); m != nil {
			usage, env = strings.TrimSuffix(usage, m[0]), "`"+m[1]+"`"
		}
		def := "`" + f.DefValue + "`"
		switch {
		case f.Name == "model-dir":
			def = "`$STATE_DIRECTORY/model`, else under `$XDG_STATE_HOME` or `~/.local/state`"
		case f.DefValue == "":
			def = "empty"
		}
		_, _ = fmt.Fprintf(
			&b,
			"| `-%s` | %s | %s | %s |\n",
			f.Name,
			env,
			def,
			link.ReplaceAllString(strings.ReplaceAll(usage, "|", `\|`), "<$0>"),
		)
	})
	return b.String()
}
