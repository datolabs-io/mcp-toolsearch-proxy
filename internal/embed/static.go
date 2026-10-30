package embed

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
)

// Files of a static model directory, as Hugging Face publishes model2vec
// models.
const (
	TokenizerFile = "tokenizer.json" //nolint:gosec // a file name, not a credential
	WeightsFile   = "model.safetensors"
)

// DefaultMaxTokens is how many pieces of a text a static model reads, as
// model2vec does by default.
const DefaultMaxTokens = 512

// maxHeader bounds the JSON header of a safetensors file.
const maxHeader = 1 << 20

// readBuffer is how much of the weights is read at a time.
const readBuffer = 1 << 20

// float32Bytes is the size of a float32 in the weights.
const float32Bytes = 4

// Static is a model2vec static embedding model: a vector for each piece of
// the tokenizer's vocabulary, so that a text's embedding is the mean of its
// pieces' vectors, scaled to unit length. Encoding needs no neural network
// and takes microseconds. The vectors are held as 8-bit integers with a
// scale per row, a quarter of the memory of the published 32-bit floats.
// It is safe for concurrent use.
type Static struct {
	name   string
	tok    *Tokenizer
	dim    int
	rows   []int8    // len(vocabulary) × dim
	scales []float32 // per row
	max    int
}

// LoadStatic loads a static model from dir, which holds TokenizerFile and
// WeightsFile. name identifies the model in cached vectors.
func LoadStatic(dir, name string) (*Static, error) {
	tf, err := os.Open(filepath.Join(dir, TokenizerFile)) //nolint:gosec // the directory the operator chose
	if err != nil {
		return nil, fmt.Errorf("embed: %w", err)
	}
	defer func() { _ = tf.Close() }()
	tok, err := ReadTokenizer(bufio.NewReader(tf))
	if err != nil {
		return nil, err
	}
	wf, err := os.Open(filepath.Join(dir, WeightsFile)) //nolint:gosec // the directory the operator chose
	if err != nil {
		return nil, fmt.Errorf("embed: %w", err)
	}
	defer func() { _ = wf.Close() }()
	return readStatic(bufio.NewReaderSize(wf, readBuffer), tok, name)
}

// tensorInfo is a tensor's entry in a safetensors header.
type tensorInfo struct {
	DType   string   `json:"dtype"`
	Shape   []int    `json:"shape"`
	Offsets [2]int64 `json:"data_offsets"`
}

// readStatic reads the "embeddings" tensor of a safetensors stream, one
// float32 row per piece of tok's vocabulary, quantizing it row by row so
// that the 32-bit matrix is never held whole.
func readStatic(r io.Reader, tok *Tokenizer, name string) (*Static, error) {
	var n uint64
	if err := binary.Read(r, binary.LittleEndian, &n); err != nil {
		return nil, fmt.Errorf("embed: read weights header: %w", err)
	}
	if n == 0 || n > maxHeader {
		return nil, fmt.Errorf("embed: weights header of %d bytes", n)
	}
	hdr := make([]byte, n)
	if _, err := io.ReadFull(r, hdr); err != nil {
		return nil, fmt.Errorf("embed: read weights header: %w", err)
	}
	var tensors map[string]json.RawMessage
	if err := json.Unmarshal(hdr, &tensors); err != nil {
		return nil, fmt.Errorf("embed: weights header: %w", err)
	}
	raw, ok := tensors["embeddings"]
	if !ok {
		return nil, errors.New("embed: weights have no embeddings tensor")
	}
	var info tensorInfo
	if err := json.Unmarshal(raw, &info); err != nil {
		return nil, fmt.Errorf("embed: embeddings tensor: %w", err)
	}
	if info.DType != "F32" || len(info.Shape) != 2 || info.Shape[0] != tok.Len() || info.Shape[1] <= 0 {
		return nil, fmt.Errorf("embed: embeddings are %s %v; want F32 [%d, dim]", info.DType, info.Shape, tok.Len())
	}
	rows, dim := info.Shape[0], info.Shape[1]
	if want := int64(rows) * int64(dim) * float32Bytes; info.Offsets[1]-info.Offsets[0] != want {
		return nil, fmt.Errorf("embed: embeddings hold %d bytes; want %d", info.Offsets[1]-info.Offsets[0], want)
	}
	if _, err := io.CopyN(io.Discard, r, info.Offsets[0]); err != nil {
		return nil, fmt.Errorf("embed: read weights: %w", err)
	}
	m := &Static{
		name: name, tok: tok, dim: dim, max: DefaultMaxTokens,
		rows: make([]int8, rows*dim), scales: make([]float32, rows),
	}
	buf := make([]byte, dim*float32Bytes)
	row := make([]float32, dim)
	for i := range rows {
		if _, err := io.ReadFull(r, buf); err != nil {
			return nil, fmt.Errorf("embed: read weights: %w", err)
		}
		for j := range row {
			row[j] = math.Float32frombits(binary.LittleEndian.Uint32(buf[j*float32Bytes:]))
		}
		m.scales[i] = quantize(row, m.rows[i*dim:(i+1)*dim])
	}
	return m, nil
}

// quantize writes row into out as 8-bit integers and returns the scale
// that maps them back.
func quantize(row []float32, out []int8) float32 {
	var peak float32
	for _, x := range row {
		peak = max(peak, float32(math.Abs(float64(x))))
	}
	if peak == 0 {
		return 0
	}
	scale := peak / math.MaxInt8
	for j, x := range row {
		out[j] = int8(math.Round(float64(x / scale)))
	}
	return scale
}

// Name identifies the model.
func (m *Static) Name() string { return m.name }

// Dim is the length of the model's vectors.
func (m *Static) Dim() int { return m.dim }

// Embed returns the embedding of each text. A text with no known piece,
// such as an empty one, gets a zero vector.
func (m *Static) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i, text := range texts {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("embed: %w", err)
		}
		out[i] = m.embed(text)
	}
	return out, nil
}

// Tokens returns the ids of the known pieces of text the model reads.
func (m *Static) Tokens(text string) []int32 {
	ids := m.tok.Encode(text, m.max)
	out := ids[:0]
	for _, id := range ids {
		if id != m.tok.Unknown() {
			out = append(out, id)
		}
	}
	return out
}

func (m *Static) embed(text string) []float32 {
	sum := make([]float64, m.dim)
	var n int
	for _, id := range m.Tokens(text) {
		scale := float64(m.scales[id])
		for j, q := range m.rows[int(id)*m.dim : (int(id)+1)*m.dim] {
			sum[j] += float64(q) * scale
		}
		n++
	}
	vec := make([]float32, m.dim)
	if n == 0 {
		return vec
	}
	var norm float64
	for _, x := range sum {
		norm += x * x
	}
	norm = math.Sqrt(norm)
	if norm == 0 {
		return vec
	}
	for j, x := range sum {
		vec[j] = float32(x / norm)
	}
	return vec
}
