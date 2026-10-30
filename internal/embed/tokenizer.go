package embed

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

// metaspace marks the start of a word in SentencePiece vocabularies.
const metaspace = "▁"

// unkPenalty is how much lower than the lowest piece an unknown character
// scores, as SentencePiece does.
const unkPenalty = 10

// paddedPunctuation are the characters the tokenizer's normalizer surrounds
// with spaces, so that each becomes a word of its own.
const paddedPunctuation = "!\"#$%&'()*+,-./:;<=>?@[\\]^_`{|}~"

// Tokenizer splits text into the pieces of a SentencePiece Unigram
// vocabulary, as the Hugging Face tokenizer of models such as BGE-M3 does:
// NFKC, punctuation set apart, whitespace collapsed, words marked with "▁",
// then the most probable segmentation of each word. It is safe for
// concurrent use.
type Tokenizer struct {
	pieces map[string]int32
	scores []float32
	unk    int32
	maxLen int     // longest piece, in runes
	unkLow float64 // score of an unknown character
}

// tokenizerFile is the part of a tokenizer.json the Tokenizer reads.
type tokenizerFile struct {
	Model struct {
		Type         string  `json:"type"`
		UnkID        *int    `json:"unk_id"`
		Vocab        []piece `json:"vocab"`
		ByteFallback bool    `json:"byte_fallback"`
	} `json:"model"`
	PreTokenizer struct {
		Type          string `json:"type"`
		Replacement   string `json:"replacement"`
		PrependScheme string `json:"prepend_scheme"`
	} `json:"pre_tokenizer"`
}

// piece is one entry of a Unigram vocabulary: ["text", log probability].
type piece struct {
	text  string
	score float64
}

func (p *piece) UnmarshalJSON(b []byte) error {
	var pair [2]json.RawMessage
	if err := json.Unmarshal(b, &pair); err != nil {
		return fmt.Errorf("vocabulary entry: %w", err)
	}
	if err := json.Unmarshal(pair[0], &p.text); err != nil {
		return fmt.Errorf("vocabulary piece: %w", err)
	}
	if err := json.Unmarshal(pair[1], &p.score); err != nil {
		return fmt.Errorf("vocabulary score: %w", err)
	}
	return nil
}

// errTokenizer is returned for a tokenizer.json of another kind.
var errTokenizer = errors.New(
	"embed: only SentencePiece Unigram tokenizers with a Metaspace pre-tokenizer are supported",
)

// ReadTokenizer reads a Hugging Face tokenizer.json.
func ReadTokenizer(r io.Reader) (*Tokenizer, error) {
	var f tokenizerFile
	if err := json.NewDecoder(r).Decode(&f); err != nil {
		return nil, fmt.Errorf("embed: read tokenizer: %w", err)
	}
	if f.Model.Type != "Unigram" || f.Model.UnkID == nil || f.Model.ByteFallback ||
		f.PreTokenizer.Type != "Metaspace" || f.PreTokenizer.Replacement != metaspace ||
		(f.PreTokenizer.PrependScheme != "" && f.PreTokenizer.PrependScheme != "always") {
		return nil, errTokenizer
	}
	if *f.Model.UnkID < 0 || *f.Model.UnkID >= len(f.Model.Vocab) {
		return nil, fmt.Errorf("embed: unknown-token id %d outside the vocabulary", *f.Model.UnkID)
	}
	t := &Tokenizer{
		pieces: make(map[string]int32, len(f.Model.Vocab)),
		scores: make([]float32, len(f.Model.Vocab)),
		unk:    int32(*f.Model.UnkID), //nolint:gosec // bounded by the vocabulary, checked above
	}
	lowest := math.Inf(1)
	for i, p := range f.Model.Vocab {
		t.scores[i] = float32(p.score)
		if _, dup := t.pieces[p.text]; !dup {
			t.pieces[p.text] = int32(i)
		}
		t.maxLen = max(t.maxLen, utf8.RuneCountInString(p.text))
		lowest = min(lowest, p.score)
	}
	t.unkLow = lowest - unkPenalty
	return t, nil
}

// Len is the size of the vocabulary.
func (t *Tokenizer) Len() int { return len(t.scores) }

// Unknown is the id of the unknown piece.
func (t *Tokenizer) Unknown() int32 { return t.unk }

// Encode returns the ids of the pieces of text, unknown ones included, at
// most limit of them; limit < 0 means no limit. Words are segmented one at
// a time until the limit is reached, and of the last only as many runes as
// the pieces left can cover, and unknownSlack more, so a long text costs
// little past the limit.
func (t *Tokenizer) Encode(text string, limit int) []int32 {
	var ids []int32
	rest := normalize(text)
	for rest != "" {
		var w string
		w, rest, _ = strings.Cut(rest, " ")
		if limit >= 0 {
			w = firstRunes(w, (limit-len(ids))*max(t.maxLen, 1)+unknownSlack)
		}
		ids = t.segment(metaspace+w, ids)
		if limit >= 0 && len(ids) >= limit {
			return ids[:limit]
		}
	}
	return ids
}

// unknownSlack is how many runes past what the pieces left can cover Encode
// still segments of a word. A run of unknown characters fuses into one
// piece of any length, so a word cut at the pieces' reach alone could lose
// what follows such a run; a run longer than this, near the limit, still
// can.
const unknownSlack = 1024

// firstRunes returns the first n runes of s.
func firstRunes(s string, n int) string {
	for i := range s {
		if n == 0 {
			return s[:i]
		}
		n--
	}
	return s
}

// normalize applies the tokenizer's normalizer: NFKC, punctuation set
// apart with spaces, runs of whitespace made one space, and both ends
// trimmed.
func normalize(s string) string {
	s = norm.NFKC.String(s)
	var b strings.Builder
	b.Grow(len(s))
	space := false
	for _, r := range s {
		switch {
		case unicode.IsSpace(r):
			space = true
		case strings.ContainsRune(paddedPunctuation, r):
			b.WriteByte(' ')
			b.WriteRune(r)
			space = true
		default:
			if space && b.Len() > 0 {
				b.WriteByte(' ')
			}
			space = false
			b.WriteRune(r)
		}
	}
	return strings.TrimLeft(b.String(), " ")
}

// segment appends to ids the most probable segmentation of w into pieces,
// found by Viterbi over the vocabulary's log probabilities. A character no
// piece covers becomes the unknown piece, and a run of them one unknown
// piece, as SentencePiece fuses them.
func (t *Tokenizer) segment(w string, ids []int32) []int32 {
	runes := []rune(w)
	n := len(runes)
	// best[i] is the best score of a segmentation of runes[:i]; from[i] the
	// start of its last piece, and id[i] that piece.
	best := make([]float64, n+1)
	from := make([]int, n+1)
	id := make([]int32, n+1)
	for i := 1; i <= n; i++ {
		best[i] = math.Inf(-1)
	}
	for start := range n {
		if math.IsInf(best[start], -1) {
			continue
		}
		covered := false
		for end := start + 1; end <= n && end-start <= t.maxLen; end++ {
			pid, ok := t.pieces[string(runes[start:end])]
			if !ok {
				continue
			}
			if end == start+1 {
				covered = true
			}
			if s := best[start] + float64(t.scores[pid]); s > best[end] {
				best[end], from[end], id[end] = s, start, pid
			}
		}
		if !covered {
			if s := best[start] + t.unkLow; s > best[start+1] {
				best[start+1], from[start+1], id[start+1] = s, start, t.unk
			}
		}
	}
	var rev []int32
	for end := n; end > 0; end = from[end] {
		if id[end] == t.unk && len(rev) > 0 && rev[len(rev)-1] == t.unk {
			continue
		}
		rev = append(rev, id[end])
	}
	for i := len(rev) - 1; i >= 0; i-- {
		ids = append(ids, rev[i])
	}
	return ids
}
