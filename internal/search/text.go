package search

import (
	"strings"
	"unicode"

	"golang.org/x/text/unicode/norm"
)

// minStem is the shortest a word may become by stemming: shorter stems
// collide too often to tell words apart. Two keeps short Lithuanian nouns
// together, such as "oras" and "oro".
const minStem = 2

// stopWords are words too common in either language to say anything about
// a tool, folded as Tokens folds them.
//
//nolint:gochecknoglobals // a constant set
var stopWords = map[string]bool{
	// English
	"a": true, "an": true, "and": true, "are": true, "as": true, "at": true, "be": true, "by": true, "can": true,
	"do": true, "for": true, "from": true, "how": true, "i": true, "in": true, "is": true, "it": true, "me": true,
	"my": true, "of": true, "on": true, "or": true, "please": true, "the": true, "this": true, "to": true,
	"what": true, "which": true, "with": true, "you": true,
	// Lithuanian, without diacritics ("iš" folds to "is", listed above)
	"ar": true, "bei": true, "ir": true, "kad": true, "kaip": true, "kas": true, "ka": true, "koks": true,
	"kokia": true, "kokie": true,
	"kur": true, "man": true, "mano": true, "nuo": true, "per": true, "prie": true, "su": true, "tai": true,
	"uz": true, "yra": true,
}

// Tokens splits s into the words BM25 compares: lower case, without
// diacritics, so that "Šildymas" and "sildymas" match, split at every
// character that is neither letter nor digit and at changes of case, so
// that "get_powerFlow" gives "get", "power" and "flow", and stemmed. Stop
// words are dropped.
func Tokens(s string) []string {
	var out []string
	for _, w := range words(s) {
		w = fold(w)
		if w == "" || stopWords[w] {
			continue
		}
		out = append(out, Stem(w))
	}
	return out
}

// words splits s at every rune that is neither a letter nor a digit, and
// between a lower-case letter and an upper-case one.
func words(s string) []string {
	var out []string
	var cur []rune
	flush := func() {
		if len(cur) > 0 {
			out = append(out, string(cur))
			cur = cur[:0]
		}
	}
	var prev rune
	for _, r := range s {
		switch {
		case !unicode.IsLetter(r) && !unicode.IsDigit(r) && !unicode.Is(unicode.Mn, r):
			flush()
		case unicode.IsUpper(r) && unicode.IsLower(prev):
			flush()
			cur = append(cur, r)
		default:
			cur = append(cur, r)
		}
		prev = r
	}
	flush()
	return out
}

// fold lowers w and strips its diacritics.
func fold(w string) string {
	var b strings.Builder
	for _, r := range norm.NFD.String(strings.ToLower(w)) {
		if !unicode.Is(unicode.Mn, r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// suffixes are the endings Stem removes, longest first so that the longest
// that fits is the one taken: Lithuanian case endings, folded, and English
// inflections. They mix the two languages on purpose: a tool's description
// and the query about it are stemmed alike, whichever language each is in,
// so an ending removed by mistake removes the same from both.
//
//nolint:gochecknoglobals // a constant list
var suffixes = []string{
	// five letters
	"iuose", "ingly", "ation",
	// four
	"iems", "ioms", "iams", "iais", "uose", "ings",
	// three
	"ing", "ies", "ams", "ems", "oms", "ims", "ais", "ose", "oje", "eje", "yje", "ius", "iai", "iui",
	// two
	"iu", "io", "ia", "ie", "ui", "es", "ed", "ly", "as", "is", "ys", "us", "ai", "os", "ei",
	// one
	"s", "a", "e", "i", "o", "u", "y",
}

// Stem removes the longest of the known endings from w, a folded word,
// keeping at least minStem letters. Words with a digit are kept as they are.
func Stem(w string) string {
	if strings.ContainsFunc(w, unicode.IsDigit) {
		return w
	}
	for _, s := range suffixes {
		if strings.HasSuffix(w, s) && len(w)-len(s) >= minStem {
			return w[:len(w)-len(s)]
		}
	}
	return w
}
