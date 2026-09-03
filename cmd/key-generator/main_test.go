package main

import (
	"context"
	"strings"
	"testing"
)

func TestToBase62(t *testing.T) {
	cases := []struct {
		name string
		in   uint64
		want string
	}{
		{"zero", 0, "0"},
		{"single digit", 9, "9"},
		{"first lowercase", 10, "a"},
		{"last lowercase", 35, "z"},
		{"first uppercase", 36, "A"},
		{"last uppercase", 61, "Z"},
		{"rolls over to two chars", 62, "10"},
		{"largest two chars", 3843, "ZZ"},
		{"smallest 7-char value", 56800235584, "1000000"},
		{"largest key the generator can produce", 3521614606207, "ZZZZZZZ"},
	}

	for _, c := range cases {
		// t.Run makes each case a named subtest, so a failure
		// reports which one broke instead of just the line number.
		t.Run(c.name, func(t *testing.T) {
			got := toBase62(c.in)
			if got != c.want {
				t.Errorf("toBase62(%d) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

func TestGenerateKey(t *testing.T) {
	for i := 0; i < 1000; i++ {
		key, err := generateKey()
		if err != nil {
			t.Fatalf("generateKey() returned error: %v", err)
		}
		if len(key) != keyLength {
			t.Errorf("generateKey() returned key of length %d, want %d", len(key), keyLength)
		}
		for _, c := range key {
			if !strings.ContainsRune(alphabet, c) {
				t.Errorf("generateKey() returned key with invalid character %q", c)
			}
		}
	}
}

func TestGenerateKeyIsRandom(t *testing.T) {
	seen := make(map[string]bool)
	for i := 0; i < 1000; i++ {
		key, err := generateKey()
		if err != nil {
			t.Fatalf("generateKey() returned error: %v", err)
		}
		if seen[key] {
			t.Errorf("generateKey() returned duplicate key: %q", key)
		}
		seen[key] = true
	}
}

func TestPadKey(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"already padded", "0000000", "0000000"},
		{"one char", "a", "000000a"},
		{"two chars", "ab", "00000ab"},
		{"three chars", "abc", "0000abc"},
		{"four chars", "abcd", "000abcd"},
		{"five chars", "abcde", "00abcde"},
		{"six chars", "abcdef", "0abcdef"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := padKey(c.in)
			if got != c.want {
				t.Errorf("padKey(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

func TestHandleRequest(t *testing.T) {
	// Empty Request on purpose: the generator must not depend on the URL.
	resp, err := handleRequest(context.Background(), Request{})
	if err != nil {
		t.Fatalf("handleRequest() returned error: %v", err)
	}
	if len(resp.Key) != keyLength {
		t.Errorf("handleRequest() key = %q, want length %d", resp.Key, keyLength)
	}
}
