package meshcore

import (
	"errors"
	"testing"
)

func testClientWithContacts() *MeshCoreClient {
	c := NewMeshCoreClient(nil)
	c.contacts = map[string]*Contact{
		"01": {Name: "Yorick 💀"},
		"02": {Name: "Короед"},
		"03": {Name: "Wire"},
		"04": {Name: "Yorick Hamlet"},
	}
	return c
}

func TestFindContactByNameLoose(t *testing.T) {
	c := testClientWithContacts()

	t.Run("exact", func(t *testing.T) {
		ct, err := c.FindContactByNameLoose("Wire")
		if err != nil || ct == nil || ct.Name != "Wire" {
			t.Errorf("got %v, %v", ct, err)
		}
	})

	t.Run("case-insensitive", func(t *testing.T) {
		ct, err := c.FindContactByNameLoose("wIRE")
		if err != nil || ct == nil || ct.Name != "Wire" {
			t.Errorf("got %v, %v", ct, err)
		}
	})

	t.Run("emoji and punctuation ignored", func(t *testing.T) {
		ct, err := c.FindContactByNameLoose("Yorick")
		if err != nil || ct == nil || ct.Name != "Yorick 💀" {
			t.Errorf("got %v, %v", ct, err)
		}
		ct, err = c.FindContactByNameLoose("короед!")
		if err != nil || ct == nil || ct.Name != "Короед" {
			t.Errorf("got %v, %v", ct, err)
		}
	})

	t.Run("unique substring", func(t *testing.T) {
		ct, err := c.FindContactByNameLoose("hamlet")
		if err != nil || ct == nil || ct.Name != "Yorick Hamlet" {
			t.Errorf("got %v, %v", ct, err)
		}
	})

	t.Run("ambiguous reports candidates", func(t *testing.T) {
		// "orick" is contained in both Yorick names.
		_, err := c.FindContactByNameLoose("orick")
		var amb *AmbiguousMatchError
		if !errors.As(err, &amb) {
			t.Fatalf("want AmbiguousMatchError, got %v", err)
		}
		if len(amb.Names) != 2 || amb.Names[0] != "Yorick Hamlet" || amb.Names[1] != "Yorick 💀" {
			t.Errorf("unexpected candidates: %v", amb.Names)
		}
	})

	t.Run("ambiguous on normalized level", func(t *testing.T) {
		// "Ann 💀" and "Ann!" differ only in decoration, so the query
		// "ANN" passes the exact and case-insensitive levels and hits two
		// candidates at the normalized level.
		c2 := NewMeshCoreClient(nil)
		c2.contacts = map[string]*Contact{
			"11": {Name: "Ann 💀"},
			"12": {Name: "Ann!"},
		}
		_, err := c2.FindContactByNameLoose("ANN")
		var amb *AmbiguousMatchError
		if !errors.As(err, &amb) || len(amb.Names) != 2 {
			t.Fatalf("want 2 candidates, got %v", err)
		}
	})

	t.Run("unknown is nil, nil", func(t *testing.T) {
		ct, err := c.FindContactByNameLoose("nobody")
		if err != nil || ct != nil {
			t.Errorf("got %v, %v", ct, err)
		}
	})

	t.Run("too-short query does not substring-match", func(t *testing.T) {
		ct, err := c.FindContactByNameLoose("wi")
		if err != nil || ct != nil {
			t.Errorf("got %v, %v", ct, err)
		}
	})

	t.Run("empty query", func(t *testing.T) {
		ct, err := c.FindContactByNameLoose("   ")
		if err != nil || ct != nil {
			t.Errorf("got %v, %v", ct, err)
		}
	})
}

func TestFindChannelByNameLoose(t *testing.T) {
	c := NewMeshCoreClient(nil)
	c.channels = map[int]*Channel{
		0: {Index: 0, Name: "public"},
		1: {Index: 1, Name: "chat 💀"},
		2: {Index: 2, Name: "", IsEmpty: true},
	}

	ct, err := c.FindChannelByNameLoose("chat")
	if err != nil || ct == nil || ct.Name != "chat 💀" {
		t.Errorf("loose: got %v, %v", ct, err)
	}
	if ch, err := c.FindChannelByNameLoose("public"); err != nil || ch == nil {
		t.Errorf("exact: got %v, %v", ch, err)
	}
	if ch, err := c.FindChannelByNameLoose("nope"); err != nil || ch != nil {
		t.Errorf("unknown: got %v, %v", ch, err)
	}
}

func TestSearchContacts(t *testing.T) {
	c := NewMeshCoreClient(nil)
	c.contacts = map[string]*Contact{
		"01": {Name: "Yorick 💀", PublicKey: []byte{0xaa, 0x01}},
		"02": {Name: "Короед", PublicKey: []byte{0xbb, 0x01}},
		"03": {Name: "Wire", PublicKey: []byte{0xcc, 0x01}},
		"04": {Name: "Yorick Hamlet", PublicKey: []byte{0xdd, 0x01}},
		"05": {Name: "Annette", PublicKey: []byte{0xaa, 0x02}},
	}

	t.Run("returns all normalized matches, sorted", func(t *testing.T) {
		hits := c.SearchContacts("yorick")
		if len(hits) != 2 || hits[0].Name != "Yorick Hamlet" || hits[1].Name != "Yorick 💀" {
			t.Errorf("got %v", names(hits))
		}
	})

	t.Run("cyrillic and decoration ignored", func(t *testing.T) {
		hits := c.SearchContacts("коро!")
		if len(hits) != 1 || hits[0].Name != "Короед" {
			t.Errorf("got %v", names(hits))
		}
	})

	t.Run("hex key prefix", func(t *testing.T) {
		hits := c.SearchContacts("aa")
		if len(hits) != 2 || hits[0].Name != "Annette" || hits[1].Name != "Yorick 💀" {
			t.Errorf("got %v", names(hits))
		}
	})

	t.Run("no match", func(t *testing.T) {
		if hits := c.SearchContacts("zzzz"); len(hits) != 0 {
			t.Errorf("got %v", names(hits))
		}
	})

	t.Run("short and empty queries return nothing", func(t *testing.T) {
		if hits := c.SearchContacts("y"); len(hits) != 0 {
			t.Errorf("single rune: got %v", names(hits))
		}
		if hits := c.SearchContacts("  "); len(hits) != 0 {
			t.Errorf("blank: got %v", names(hits))
		}
	})
}

func names(cs []*Contact) []string {
	out := make([]string, len(cs))
	for i, c := range cs {
		out[i] = c.Name
	}
	return out
}

func TestNormalizeName(t *testing.T) {
	cases := map[string]string{
		"Yorick 💀":   "yorick",
		"Короед":     "короед",
		"  Wire  ":   "wire",
		"Node-2_v3":  "node2v3",
		"💀":          "",
		"John Smith": "johnsmith",
	}
	for in, want := range cases {
		if got := normalizeName(in); got != want {
			t.Errorf("normalizeName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestTranslitLat(t *testing.T) {
	cases := map[string]string{
		"йорик":     "yorik",
		"короед":    "koroed",
		"ёжик":      "yozhik",
		"александр": "aleksandr",
		"yorick":    "yorick",
		"node2":     "node2",
		"":          "",
	}
	for in, want := range cases {
		if got := translitLat(in); got != want {
			t.Errorf("translitLat(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestLevenshtein(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"yorik", "yorick", 1},
		{"kitten", "sitting", 3},
		{"same", "same", 0},
		{"abc", "", 3},
		{"aleksandr", "alexander", 3},
	}
	for _, c := range cases {
		if got := levenshtein([]rune(c.a), []rune(c.b)); got != c.want {
			t.Errorf("levenshtein(%q, %q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

func TestFuzzyNameMatch(t *testing.T) {
	cases := []struct {
		name, query string
		want        bool
	}{
		{"yorick", "йорик", true},       // cross-language, edit distance 1
		{"йорик", "yorik", true},        // and the reverse direction
		{"yorickhamlet", "йорик", true}, // fuzzy window inside a long name
		{"koroyed", "короед", true},     // wait: koroyed vs koroed — dist 1
		{"koroed", "короед", true},      // exact after translit
		{"wire", "вайр", false},         // too different
		{"ann", "ан", false},            // below the 2-rune minimum
		{"yorick", "wladorb", false},    // noise
	}
	for _, c := range cases {
		if got := fuzzyNameMatch(c.name, c.query); got != c.want {
			t.Errorf("fuzzyNameMatch(%q, %q) = %v, want %v", c.name, c.query, got, c.want)
		}
	}
}

func TestRussianQueryFindsLatinContact(t *testing.T) {
	c := NewMeshCoreClient(nil)
	c.contacts = map[string]*Contact{
		"01": {Name: "Yorick 💀"},
		"02": {Name: "Wire"},
		"03": {Name: "Короед"},
	}

	t.Run("search: Йорик finds Yorick 💀", func(t *testing.T) {
		hits := c.SearchContacts("Йорик")
		if len(hits) != 1 || hits[0].Name != "Yorick 💀" {
			t.Errorf("got %v", names(hits))
		}
	})

	t.Run("search: both Yoricks found in Russian", func(t *testing.T) {
		c.contacts["04"] = &Contact{Name: "Yorick Hamlet"}
		hits := c.SearchContacts("Йорик")
		if len(hits) != 2 || hits[0].Name != "Yorick Hamlet" || hits[1].Name != "Yorick 💀" {
			t.Errorf("got %v", names(hits))
		}
		delete(c.contacts, "04")
	})

	t.Run("find (send path): Йорик resolves to Yorick 💀", func(t *testing.T) {
		ct, err := c.FindContactByNameLoose("Йорик")
		if err != nil || ct == nil || ct.Name != "Yorick 💀" {
			t.Errorf("got %v, %v", ct, err)
		}
	})

	t.Run("find: latin query finds cyrillic name", func(t *testing.T) {
		ct, err := c.FindContactByNameLoose("koroyed")
		if err != nil || ct == nil || ct.Name != "Короед" {
			t.Errorf("got %v, %v", ct, err)
		}
	})

	t.Run("search: cyrillic names still found directly", func(t *testing.T) {
		hits := c.SearchContacts("короед")
		if len(hits) != 1 || hits[0].Name != "Короед" {
			t.Errorf("got %v", names(hits))
		}
	})
}
