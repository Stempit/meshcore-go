package meshcore

// Loose name matching for contacts and channels. A spoken or typed query
// ("Yorick") must find contacts whose stored name carries extra decoration
// ("Yorick 💀"): matching progresses from exact through case-insensitive to a
// normalized form (letters and digits only), then to a unique substring hit.
// When a matching level hits several items, the lookup fails with
// AmbiguousMatchError listing the candidates.

import (
	"sort"
	"strings"
	"unicode"
)

// AmbiguousMatchError reports that a name lookup matched several items
// equally well; Names holds the candidates so the caller can ask the user to
// disambiguate.
type AmbiguousMatchError struct {
	Names []string
}

func (e *AmbiguousMatchError) Error() string {
	return "ambiguous name match: " + strings.Join(e.Names, ", ")
}

// normalizeName reduces a name to lowercase letters and digits, dropping
// emoji, punctuation and spaces — the difference between the stored
// "Yorick 💀" and its spoken form "yorick". unicode.IsLetter/IsDigit keep
// non-Latin scripts (Cyrillic etc.) intact.
func normalizeName(s string) string {
	var b strings.Builder
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(unicode.ToLower(r))
		}
	}
	return b.String()
}

// namedEntry is one matchable named item with precomputed comparison forms.
type namedEntry[T any] struct {
	name  string
	lower string
	norm  string
	value T
}

// minSubstringRunes is the shortest query that may match by substring; below
// it a stray letter would match half the contact list.
const minSubstringRunes = 3

// findByNameLoose runs the matching cascade over entries. It returns the
// single item of the first level that matches exactly one entry; a level
// matching several yields *AmbiguousMatchError, no match at all yields
// (zero, nil). The phonetic-fuzzy level runs last, so it only kicks in when
// every exact form missed.
func findByNameLoose[T any](entries []namedEntry[T], query string) (T, error) {
	var zero T
	query = strings.TrimSpace(query)
	if query == "" {
		return zero, nil
	}
	lower := strings.ToLower(query)
	norm := normalizeName(query)

	levels := []func(namedEntry[T]) bool{
		func(e namedEntry[T]) bool { return e.name == query },
		func(e namedEntry[T]) bool { return e.lower == lower },
		func(e namedEntry[T]) bool { return e.norm == norm },
		func(e namedEntry[T]) bool {
			return len([]rune(norm)) >= minSubstringRunes && strings.Contains(e.norm, norm)
		},
		// Phonetic fuzzy fallback: "Йорик" finds "Yorick 💀", "yorik" would
		// find "Йорик 💀".
		func(e namedEntry[T]) bool { return fuzzyNameMatch(e.norm, norm) },
	}

	for _, level := range levels {
		var hits []namedEntry[T]
		for _, e := range entries {
			if level(e) {
				hits = append(hits, e)
			}
		}
		switch len(hits) {
		case 1:
			return hits[0].value, nil
		case 0:
			continue
		default:
			names := make([]string, 0, len(hits))
			for _, h := range hits {
				names = append(names, h.name)
			}
			sort.Strings(names)
			return zero, &AmbiguousMatchError{Names: names}
		}
	}
	return zero, nil
}

// FindContactByNameLoose resolves a contact by name, forgiving case, emoji
// and punctuation ("yorick" finds "Yorick 💀"; a unique substring like
// "yorick ha" finds "Yorick Hamlet 💀"). Returns (*Contact, nil) when nothing
// matches, and *AmbiguousMatchError when several do.
func (c *MeshCoreClient) FindContactByNameLoose(name string) (*Contact, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	entries := make([]namedEntry[*Contact], 0, len(c.contacts))
	for _, ct := range c.contacts {
		entries = append(entries, namedEntry[*Contact]{ct.Name, strings.ToLower(ct.Name), normalizeName(ct.Name), ct})
	}
	return findByNameLoose(entries, name)
}

// FindChannelByNameLoose resolves a channel by name with the same forgiving
// cascade as FindContactByNameLoose, skipping empty channel slots.
func (c *MeshCoreClient) FindChannelByNameLoose(name string) (*Channel, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	var entries []namedEntry[*Channel]
	for _, ch := range c.channels {
		if ch.IsEmpty {
			continue
		}
		entries = append(entries, namedEntry[*Channel]{ch.Name, strings.ToLower(ch.Name), normalizeName(ch.Name), ch})
	}
	return findByNameLoose(entries, name)
}

// minSearchRunes is the shortest query SearchContacts accepts; a single rune
// would sweep half a large contact list.
const minSearchRunes = 2

// cyrPhonetic maps Cyrillic letters to their Latin phonetic equivalents, so a
// name spelled as the assistant heard it in Russian ("Йорик") matches the
// stored Latin spelling ("Yorick 💀"). Digraphs (ж→zh, я→ya, …) expand to two
// runes. Soft/hard signs are silent and drop out. This is PHONETIC, not the
// visual homoglyph squeeze the sending side applies (see the fit package in
// meshcore-companion).
var cyrPhonetic = map[rune]string{
	'а': "a", 'б': "b", 'в': "v", 'г': "g", 'д': "d", 'е': "e", 'ё': "yo",
	'ж': "zh", 'з': "z", 'и': "i", 'й': "y", 'к': "k", 'л': "l", 'м': "m",
	'н': "n", 'о': "o", 'п': "p", 'р': "r", 'с': "s", 'т': "t", 'у': "u",
	'ф': "f", 'х': "kh", 'ц': "ts", 'ч': "ch", 'ш': "sh", 'щ': "shch",
	'ъ': "", 'ы': "y", 'ь': "", 'э': "e", 'ю': "yu", 'я': "ya",
}

// translitLat renders a normalized (lowercase) name in Latin phonetics:
// Cyrillic letters become their sound equivalents, everything else (already
// Latin names, digits) passes through. Both sides of a comparison go through
// it, so either language finds the other.
func translitLat(s string) string {
	var b strings.Builder
	for _, r := range s {
		if tr, ok := cyrPhonetic[r]; ok {
			b.WriteString(tr)
		} else {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// levenshtein is the classic edit distance (insert/delete/replace) over runes.
func levenshtein(a, b []rune) int {
	prev := make([]int, len(b)+1)
	cur := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev, cur = cur, prev
	}
	return prev[len(b)]
}

// hamming is the substitution-only distance between equal-length runes.
func hamming(a, b []rune) int {
	n := 0
	for i := range a {
		if a[i] != b[i] {
			n++
		}
	}
	return n
}

// fuzzyAllow is the edit budget for a fuzzy name match: one slip on short
// names, more headroom as they grow ("yorik"≈"yorick" needs 1,
// "aleksandr"≈"alexander" needs 3).
func fuzzyAllow(queryLen int) int {
	return max(1, queryLen/3)
}

// minFuzzyRunes is the shortest translit query the fuzzy (edit-distance)
// levels accept: below it a budget of even 1 substitution matches random
// short windows ("aa" ≈ "am"), drowning real hits in noise.
const minFuzzyRunes = 3

// fuzzyNameMatch reports whether a stored normalized name matches the
// normalized query phonetically: exact substring after transliteration, a
// whole-name edit within the budget, or a same-length window of the name
// within the budget (so "yorik" hits inside "yorickhamlet"). Both sides are
// transliterated first, hence it is language-agnostic: "Йорик" matches
// "Yorick 💀" and "yorik" would match "Йорик 💀".
func fuzzyNameMatch(nameNorm, queryNorm string) bool {
	q := []rune(translitLat(queryNorm))
	if len(q) < minFuzzyRunes {
		return false
	}
	n := translitLat(nameNorm)
	if strings.Contains(n, string(q)) {
		return true
	}
	allow := fuzzyAllow(len(q))
	if levenshtein([]rune(n), q) <= allow {
		return true
	}
	rn := []rune(n)
	if len(rn) <= len(q) {
		return false
	}
	for i := 0; i+len(q) <= len(rn); i++ {
		if hamming(rn[i:i+len(q)], q) <= allow {
			return true
		}
	}
	return false
}

// SearchContacts returns all contacts whose name contains the query — case,
// emoji and punctuation ignored ("yorick" matches "Yorick 💀" and "Yorick
// Hamlet") — or whose public key starts with it (hex). Names also match
// phonetically across languages: the Russian spelling finds the Latin name
// and vice versa ("Йорик" matches "Yorick 💀"). Results are sorted by
// name. Unlike FindContactByNameLoose it never fails on multiple matches:
// returning all of them is the point. An empty or too-short query returns nil.
func (c *MeshCoreClient) SearchContacts(query string) []*Contact {
	c.mu.RLock()
	defer c.mu.RUnlock()

	norm := normalizeName(strings.TrimSpace(query))
	if len([]rune(norm)) < minSearchRunes {
		return nil
	}
	queryHex := strings.ToLower(strings.TrimSpace(query))

	var hits []*Contact
	for _, ct := range c.contacts {
		nameNorm := normalizeName(ct.Name)
		if strings.Contains(nameNorm, norm) || fuzzyNameMatch(nameNorm, norm) {
			hits = append(hits, ct)
			continue
		}
		if strings.HasPrefix(pubKeyHex(ct.PublicKey), queryHex) {
			hits = append(hits, ct)
		}
	}
	sort.Slice(hits, func(i, j int) bool { return hits[i].Name < hits[j].Name })
	return hits
}
