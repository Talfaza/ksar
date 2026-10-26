package llm

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// TestTokenizerMatchesReference compares against ids from the reference
// Hugging Face tokenizers library (written to tok_want.json by a script),
// when KSAR_LLM_TESTDATA points at them and the model files.
func TestTokenizerMatchesReference(t *testing.T) {
	dir := os.Getenv("KSAR_LLM_TESTDATA")
	if dir == "" {
		t.Skip("KSAR_LLM_TESTDATA not set")
	}
	tok, err := LoadTokenizer(filepath.Join(dir, "smol", "tokenizer.json"))
	if err != nil {
		t.Fatal(err)
	}
	var cases []string
	var want [][]int
	read := func(name string, v any) {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(b, v); err != nil {
			t.Fatal(err)
		}
	}
	read("tok_cases.json", &cases)
	read("tok_want.json", &want)
	for i, c := range cases {
		got := tok.Encode(c)
		if fmt.Sprint(got) != fmt.Sprint(want[i]) {
			t.Errorf("%q:\n got %v\nwant %v", c, got, want[i])
		}
		if dec := tok.Decode(got); dec != stripSpecial(tok, c) {
			t.Errorf("decode %q = %q", c, dec)
		}
	}
}

func stripSpecial(t *Tokenizer, s string) string {
	for _, sp := range t.sortedSpecial() {
		for {
			i := indexOf(s, sp)
			if i < 0 {
				break
			}
			s = s[:i] + s[i+len(sp):]
		}
	}
	return s
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
