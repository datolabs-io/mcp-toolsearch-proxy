package embed_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/datolabs-io/mcp-toolsearch-proxy/internal/embed"
)

// vocab is a tiny Unigram vocabulary: pieces and log probabilities.
func vocab() [][2]any {
	return [][2]any{
		{"[PAD]", -10.0}, {"[UNK]", -10.0},
		{"▁", -3.0}, {"▁hello", -2.0}, {"▁he", -4.0}, {"llo", -4.0}, {"▁world", -2.5},
		{"▁w", -5.0}, {"or", -5.0}, {"ld", -5.0}, {"!", -3.0}, {"▁!", -2.0},
		{"▁ša", -3.0}, {"ka", -3.0}, {"▁šaka", -6.5}, {"▁fi", -3.0},
	}
}

func tokenizerJSON(t *testing.T) []byte {
	t.Helper()
	raw, err := json.Marshal(map[string]any{
		"model":         map[string]any{"type": "Unigram", "unk_id": 1, "vocab": vocab(), "byte_fallback": false},
		"pre_tokenizer": map[string]any{"type": "Metaspace", "replacement": "▁", "prepend_scheme": "always"},
	})
	require.NoError(t, err)
	return raw
}

func tokenizer(t *testing.T) *embed.Tokenizer {
	t.Helper()
	tok, err := embed.ReadTokenizer(bytes.NewReader(tokenizerJSON(t)))
	require.NoError(t, err)
	return tok
}

func TestTokenizerPicksMostProbableSegmentation(t *testing.T) {
	t.Parallel()
	tok := tokenizer(t)

	// "▁hello" (-2) beats "▁he" + "llo" (-8); "▁ša" + "ka" (-6) beats "▁šaka" (-6.5).
	assert.Equal(t, []int32{3, 6}, tok.Encode("hello world", -1))
	assert.Equal(t, []int32{12, 13}, tok.Encode("šaka", -1))
}

func TestTokenizerNormalizes(t *testing.T) {
	t.Parallel()
	tok := tokenizer(t)

	// NFKC turns the ligature into "fi"; punctuation becomes a word of its own,
	// and whitespace runs collapse.
	assert.Equal(t, []int32{15}, tok.Encode("ﬁ", -1))
	assert.Equal(t, []int32{3, 11, 6}, tok.Encode("  hello!\t\n world ", -1))
}

func TestTokenizerFusesUnknownCharacters(t *testing.T) {
	t.Parallel()
	tok := tokenizer(t)

	assert.Equal(t, []int32{3, 2, 1}, tok.Encode("hello zzz", -1))
	assert.Equal(t, int32(1), tok.Unknown())
}

func TestTokenizerLimit(t *testing.T) {
	t.Parallel()
	tok := tokenizer(t)

	assert.Equal(t, []int32{3}, tok.Encode("hello world hello", 1))
	assert.Empty(t, tok.Encode("", -1))
}

// A word segments in time and memory in proportion to its length, so once
// the limit is near only as much of a long word is segmented as the pieces
// left can cover.
//
//nolint:paralleltest // it measures the memory one call allocates, so it runs alone
func TestTokenizerLimitBoundsALongWord(t *testing.T) {
	tok := tokenizer(t)
	word := strings.Repeat("hello", 200_000)

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	ids := tok.Encode("hello "+word, 4)
	runtime.ReadMemStats(&after)

	assert.Len(t, ids, 4)
	assert.Equal(t, int32(3), ids[0])
	assert.Less(t, after.TotalAlloc-before.TotalAlloc, uint64(8<<20),
		"a million-rune word is not segmented whole for three pieces")
}

// A run of unknown characters fuses into one piece, however long, so the
// bound on a long word leaves room for one before the pieces after it.
func TestTokenizerLimitKeepsPiecesAfterAnUnknownRun(t *testing.T) {
	t.Parallel()
	tok := tokenizer(t)
	text := strings.Repeat("z", 25) + "llo world"

	assert.Equal(t, []int32{2, 1, 5, 6}, tok.Encode(text, -1))
	assert.Equal(t, []int32{2, 1, 5, 6}, tok.Encode(text, 4))
}

func TestReadTokenizerRefusesOtherKinds(t *testing.T) {
	t.Parallel()

	for _, raw := range []string{
		`{"model":{"type":"BPE","unk_id":0,"vocab":[["a",0]]},"pre_tokenizer":{"type":"Metaspace","replacement":"▁"}}`,
		`{"model":{"type":"Unigram","vocab":[["a",0]]},"pre_tokenizer":{"type":"Metaspace","replacement":"▁"}}`,
		`{"model":{"type":"Unigram","unk_id":5,"vocab":[["a",0]]},"pre_tokenizer":{"type":"Metaspace","replacement":"▁"}}`,
		`{"model":{"type":"Unigram","unk_id":0,"vocab":[["a",0]]},"pre_tokenizer":{"type":"ByteLevel"}}`,
		`{"model":{"type":"Unigram","unk_id":0,"vocab":[["a"]]},"pre_tokenizer":{"type":"Metaspace","replacement":"▁"}}`,
		`not json`,
	} {
		_, err := embed.ReadTokenizer(strings.NewReader(raw))
		assert.Error(t, err, raw)
	}
}

// safetensors returns a safetensors file holding rows as its embeddings.
func safetensors(t *testing.T, dtype string, rows [][]float32) []byte {
	t.Helper()
	dim := len(rows[0])
	hdr, err := json.Marshal(map[string]any{
		"__metadata__": map[string]string{"format": "pt"},
		"embeddings": map[string]any{
			"dtype":        dtype,
			"shape":        []int{len(rows), dim},
			"data_offsets": []int{0, len(rows) * dim * 4},
		},
	})
	require.NoError(t, err)
	var b bytes.Buffer
	require.NoError(t, binary.Write(&b, binary.LittleEndian, uint64(len(hdr))))
	b.Write(hdr)
	for _, r := range rows {
		for _, x := range r {
			require.NoError(t, binary.Write(&b, binary.LittleEndian, math.Float32bits(x)))
		}
	}
	return b.Bytes()
}

// modelDir writes a static model with a vector per piece of vocab(): piece
// i points along axis i%4, except that "▁world" points along axis 0 and
// "▁hello" along axis 1.
func modelDir(t *testing.T) string {
	t.Helper()
	rows := make([][]float32, len(vocab()))
	for i := range rows {
		rows[i] = make([]float32, 4)
		rows[i][i%4] = float32(i + 1)
	}
	rows[3] = []float32{0, 2, 0, 0}
	rows[6] = []float32{-3, 0, 0, 0}
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, embed.TokenizerFile), tokenizerJSON(t), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, embed.WeightsFile), safetensors(t, "F32", rows), 0o600))
	return dir
}

func TestStaticEmbedsTheMeanOfPieces(t *testing.T) {
	t.Parallel()
	m, err := embed.LoadStatic(modelDir(t), "tiny")
	require.NoError(t, err)

	vecs, err := m.Embed(t.Context(), []string{"hello", "hello world", "zzz", ""})
	require.NoError(t, err)

	assert.Equal(t, "tiny", m.Name())
	assert.Equal(t, 4, m.Dim())
	assert.InDeltaSlice(t, []float32{0, 1, 0, 0}, vecs[0], 1e-6)
	// Unit vectors (0,1) and (-1,0), each piece weighed by its own length
	// before the mean is scaled to unit length: (-3, 2)/√13.
	assert.InDeltaSlice(t, []float32{-3 / float32(math.Sqrt(13)), 2 / float32(math.Sqrt(13)), 0, 0}, vecs[1], 1e-2)
	assert.Equal(t, []float32{0, 0, 1, 0}, vecs[2], "the unknown piece is left out; the word marker is known")
	assert.Equal(t, []float32{0, 0, 0, 0}, vecs[3])
}

func TestStaticRefusesMismatchedWeights(t *testing.T) {
	t.Parallel()

	for name, weights := range map[string][]byte{
		"wrong dtype":     safetensors(t, "F16", make([][]float32, len(vocab()))[:1:1]),
		"wrong row count": safetensors(t, "F32", [][]float32{{1, 2}}),
		"empty":           {},
		"huge header":     binary.LittleEndian.AppendUint64(nil, 1<<40),
	} {
		dir := modelDir(t)
		require.NoError(t, os.WriteFile(filepath.Join(dir, embed.WeightsFile), weights, 0o600))
		_, err := embed.LoadStatic(dir, "tiny")
		assert.Error(t, err, name)
	}
}

func TestStaticEmbedStopsWhenCancelled(t *testing.T) {
	t.Parallel()
	m, err := embed.LoadStatic(modelDir(t), "tiny")
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	_, err = m.Embed(ctx, []string{"hello"})
	assert.Error(t, err)
}

func TestFetchVerifiesDigests(t *testing.T) {
	t.Parallel()
	files := map[string][]byte{embed.TokenizerFile: []byte("tok"), embed.WeightsFile: []byte("weights")}
	var served int
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		served++
		body, ok := files[filepath.Base(r.URL.Path)]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(body)
	}))
	t.Cleanup(ts.Close)
	src := embed.Source{Name: "test", BaseURL: ts.URL + "/m/", SHA256: map[string]string{}}
	for name, body := range files {
		sum := sha256.Sum256(body)
		src.SHA256[name] = hex.EncodeToString(sum[:])
	}
	dir := filepath.Join(t.TempDir(), "model")

	require.NoError(t, embed.Fetch(t.Context(), ts.Client(), src, dir))
	require.NoError(t, embed.Verify(src, dir))
	info, err := os.Stat(filepath.Join(dir, embed.WeightsFile))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o644), info.Mode().Perm(), "readable by whoever runs the proxy")
	require.NoError(t, embed.Fetch(t.Context(), ts.Client(), src, dir))
	assert.Equal(t, 2, served, "files already fetched are not fetched again")

	bad := embed.Source{Name: "test", BaseURL: src.BaseURL, SHA256: map[string]string{
		embed.TokenizerFile: src.SHA256[embed.TokenizerFile], embed.WeightsFile: fmt.Sprintf("%064d", 0),
	}}
	other := filepath.Join(t.TempDir(), "model")
	require.Error(t, embed.Fetch(t.Context(), ts.Client(), bad, other))
	_, err = os.Stat(filepath.Join(other, embed.WeightsFile))
	require.ErrorIs(t, err, os.ErrNotExist, "a file with the wrong digest is not kept")
	entries, err := os.ReadDir(other)
	require.NoError(t, err)
	assert.Len(t, entries, 1, "nor its temporary copy")
	assert.Error(t, embed.Verify(bad, dir))
}

func FuzzEncode(f *testing.F) {
	raw, err := json.Marshal(map[string]any{
		"model":         map[string]any{"type": "Unigram", "unk_id": 1, "vocab": vocab(), "byte_fallback": false},
		"pre_tokenizer": map[string]any{"type": "Metaspace", "replacement": "▁", "prepend_scheme": "always"},
	})
	if err != nil {
		f.Fatal(err)
	}
	tok, err := embed.ReadTokenizer(bytes.NewReader(raw))
	if err != nil {
		f.Fatal(err)
	}
	for _, s := range []string{"", "hello world", "šaka!", "ﬁ  \t", "\xff\xfe", "▁▁"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		for _, id := range tok.Encode(s, 64) {
			if id < 0 || int(id) >= tok.Len() {
				t.Fatalf("id %d outside the vocabulary for %q", id, s)
			}
		}
	})
}
