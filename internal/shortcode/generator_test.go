package shortcode

import (
	"errors"
	"strings"
	"testing"
)

func TestRandomLengthAndAlphabet(t *testing.T) {
	for _, n := range []int{0, -3, 1, 7, 12, 32} {
		want := n
		if n <= 0 {
			want = 7
		}
		g := NewRandom(n)
		for i := 0; i < 200; i++ {
			c, err := g.Generate()
			if err != nil {
				t.Fatal(err)
			}
			if len(c) != want {
				t.Fatalf("длина %d, ожидалось %d", len(c), want)
			}
			for j := 0; j < len(c); j++ {
				if !strings.ContainsRune(alphabet, rune(c[j])) {
					t.Fatalf("символ %q вне алфавита", c[j])
				}
			}
		}
	}
}

func TestRandomNoCollisions(t *testing.T) {
	g := NewRandom(10)
	seen := make(map[string]struct{}, 100000)
	for i := 0; i < 100000; i++ {
		c, err := g.Generate()
		if err != nil {
			t.Fatal(err)
		}
		if _, dup := seen[c]; dup {
			t.Fatalf("коллизия на итерации %d: %s", i, c)
		}
		seen[c] = struct{}{}
	}
}

func TestRandomUniform(t *testing.T) {
	g := NewRandom(10)
	counts := map[byte]int{}
	const codes = 50000
	for i := 0; i < codes; i++ {
		c, _ := g.Generate()
		for j := 0; j < len(c); j++ {
			counts[c[j]]++
		}
	}
	total := codes * 10
	exp := float64(total) / float64(len(alphabet))
	for i := 0; i < len(alphabet); i++ {
		got := float64(counts[alphabet[i]])
		if got < exp*0.9 || got > exp*1.1 {
			t.Errorf("символ %q встретился %.0f раз, ожидалось около %.0f", alphabet[i], got, exp)
		}
	}
}

func TestValidateAlias(t *testing.T) {
	tests := []struct {
		in   string
		want error
	}{
		{"abc", nil},
		{"My-Link_1", nil},
		{"a1_", nil},
		{strings.Repeat("a", 32), nil},
		{"ab", ErrAliasLength},
		{"", ErrAliasLength},
		{strings.Repeat("a", 33), ErrAliasLength},
		{"-abc", ErrAliasStart},
		{"_abc", ErrAliasStart},
		{"ab c", ErrAliasChars},
		{"ab.c", ErrAliasChars},
		{"привет", ErrAliasChars},
		{"api", ErrAliasReserved},
		{"API", ErrAliasReserved},
		{"Admin", ErrAliasReserved},
		{"favicon.ico", ErrAliasChars},
		{"dashboard", ErrAliasReserved},
	}
	for _, tt := range tests {
		got := ValidateAlias(tt.in)
		if !errors.Is(got, tt.want) {
			t.Errorf("ValidateAlias(%q) = %v, ожидалось %v", tt.in, got, tt.want)
		}
	}
}

func TestLooksValid(t *testing.T) {
	tests := []struct {
		in   string
		want bool
	}{
		{"a", true},
		{"abc-DEF_09", true},
		{"-x", true},
		{strings.Repeat("z", 32), true},
		{"", false},
		{strings.Repeat("z", 33), false},
		{"a/b", false},
		{"a b", false},
		{"a.b", false},
		{"ключ", false},
	}
	for _, tt := range tests {
		if got := LooksValid(tt.in); got != tt.want {
			t.Errorf("LooksValid(%q) = %v, ожидалось %v", tt.in, got, tt.want)
		}
	}
}

func BenchmarkGenerate(b *testing.B) {
	g := NewRandom(7)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := g.Generate(); err != nil {
			b.Fatal(err)
		}
	}
}
