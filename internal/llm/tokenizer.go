package llm

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Tokenizer is a byte-level BPE tokenizer (GPT-2 style) read from a
// Hugging Face tokenizer.json, with SmolLM2's pre-tokenization: single
// digits, then the GPT-2 split pattern.
type Tokenizer struct {
	vocab   map[string]int
	tokens  []string
	ranks   map[[2]string]int
	special map[string]int
	byteEnc [256]rune
	byteDec map[rune]byte
}

// LoadTokenizer reads tokenizer.json.
func LoadTokenizer(path string) (*Tokenizer, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var tj struct {
		AddedTokens []struct {
			ID      int    `json:"id"`
			Content string `json:"content"`
		} `json:"added_tokens"`
		Model struct {
			Type   string            `json:"type"`
			Vocab  map[string]int    `json:"vocab"`
			Merges []json.RawMessage `json:"merges"`
		} `json:"model"`
	}
	if err := json.Unmarshal(b, &tj); err != nil {
		return nil, fmt.Errorf("tokenizer.json: %w", err)
	}
	if tj.Model.Type != "BPE" {
		return nil, fmt.Errorf("tokenizer type %q, want BPE", tj.Model.Type)
	}
	t := &Tokenizer{vocab: tj.Model.Vocab, ranks: map[[2]string]int{}, special: map[string]int{}, byteDec: map[rune]byte{}}
	t.tokens = make([]string, len(t.vocab))
	for s, id := range t.vocab {
		if id < 0 || id >= len(t.tokens) {
			return nil, fmt.Errorf("token id %d out of range", id)
		}
		t.tokens[id] = s
	}
	for i, m := range tj.Model.Merges {
		// Merges are "a b" strings (older files) or ["a", "b"] pairs.
		var pair [2]string
		var s string
		if json.Unmarshal(m, &s) == nil {
			a, b, ok := strings.Cut(s, " ")
			if !ok {
				return nil, fmt.Errorf("bad merge %q", s)
			}
			pair = [2]string{a, b}
		} else if err := json.Unmarshal(m, &pair); err != nil {
			return nil, fmt.Errorf("bad merge %s", m)
		}
		t.ranks[pair] = i
	}
	for _, a := range tj.AddedTokens {
		t.special[a.Content] = a.ID
	}
	// GPT-2's reversible map from bytes to printable characters.
	n := 0
	for b := 0; b < 256; b++ {
		if b >= '!' && b <= '~' || b >= 0xa1 && b <= 0xac || b >= 0xae && b <= 0xff {
			t.byteEnc[b] = rune(b)
		} else {
			t.byteEnc[b] = rune(256 + n)
			n++
		}
		t.byteDec[t.byteEnc[b]] = byte(b)
	}
	return t, nil
}

// Special returns the id of a special token such as <|im_end|>.
func (t *Tokenizer) Special(s string) int {
	if id, ok := t.special[s]; ok {
		return id
	}
	return -1
}

// Encode turns text into token ids. Special tokens written literally in
// the text (like <|im_start|>) become their ids.
func (t *Tokenizer) Encode(text string) []int {
	var ids []int
	for len(text) > 0 {
		// Find the earliest special token.
		at, which := -1, ""
		for s := range t.special {
			if i := strings.Index(text, s); i >= 0 && (at < 0 || i < at || i == at && len(s) > len(which)) {
				at, which = i, s
			}
		}
		chunk := text
		if at >= 0 {
			chunk = text[:at]
		}
		for _, piece := range pretokenize(chunk) {
			ids = append(ids, t.bpe(piece)...)
		}
		if at < 0 {
			break
		}
		ids = append(ids, t.special[which])
		text = text[at+len(which):]
	}
	return ids
}

// Decode turns ids back into text (special tokens are skipped).
func (t *Tokenizer) Decode(ids []int) string {
	var b []byte
	for _, id := range ids {
		if id < 0 || id >= len(t.tokens) {
			continue
		}
		s := t.tokens[id]
		if _, ok := t.special[s]; ok {
			continue
		}
		for _, r := range s {
			if c, ok := t.byteDec[r]; ok {
				b = append(b, c)
			}
		}
	}
	return string(b)
}

// bpe encodes one pre-token.
func (t *Tokenizer) bpe(piece string) []int {
	var parts []string
	for _, c := range []byte(piece) {
		parts = append(parts, string(t.byteEnc[c]))
	}
	for len(parts) > 1 {
		best, at := -1, -1
		for i := 0; i+1 < len(parts); i++ {
			if r, ok := t.ranks[[2]string{parts[i], parts[i+1]}]; ok && (best < 0 || r < best) {
				best, at = r, i
			}
		}
		if at < 0 {
			break
		}
		parts[at] += parts[at+1]
		parts = append(parts[:at+1], parts[at+2:]...)
	}
	ids := make([]int, 0, len(parts))
	for _, p := range parts {
		if id, ok := t.vocab[p]; ok {
			ids = append(ids, id)
		} else { // unknown merge result: fall back to its bytes
			for _, r := range p {
				if id, ok := t.vocab[string(r)]; ok {
					ids = append(ids, id)
				}
			}
		}
	}
	return ids
}

// pretokenize splits text like SmolLM2's tokenizer: every digit on its
// own, then the GPT-2 pattern
// 's|'t|'re|'ve|'m|'ll|'d| ?\pL+| ?\pN+| ?[^\s\pL\pN]+|\s+(?!\S)|\s+
// (written by hand: Go's regexp has no lookahead).
func pretokenize(text string) []string {
	var out []string
	for len(text) > 0 {
		n := gpt2Match(text)
		out = append(out, text[:n])
		text = text[n:]
	}
	return out
}

func isL(r rune) bool { return unicode.IsLetter(r) }
func isN(r rune) bool { return unicode.IsNumber(r) }
func isS(r rune) bool { return unicode.IsSpace(r) }

// gpt2Match returns the length of the next pre-token at the start of s.
func gpt2Match(s string) int {
	for _, c := range []string{"'s", "'t", "'re", "'ve", "'m", "'ll", "'d"} {
		if strings.HasPrefix(s, c) {
			return len(c)
		}
	}
	r, size := utf8.DecodeRuneInString(s)
	start := 0
	// An optional leading space joins the next token, except a digit:
	// the Digits pre-tokenizer has already cut digits apart from it.
	if r == ' ' && len(s) > 1 {
		r2, size2 := utf8.DecodeRuneInString(s[1:])
		if !isS(r2) && !isN(r2) {
			start, r, size = 1, r2, size2
		}
	}
	run := func(from int, pred func(rune) bool) int {
		i := from
		for i < len(s) {
			c, n := utf8.DecodeRuneInString(s[i:])
			if !pred(c) {
				break
			}
			i += n
		}
		return i
	}
	switch {
	case isL(r):
		return run(start, isL)
	case isN(r):
		// Digits: one per token (the Digits pre-tokenizer runs first).
		return start + size
	case !isS(r):
		return run(start, func(c rune) bool { return !isS(c) && !isL(c) && !isN(c) })
	}
	// Whitespace: \s+(?!\S) takes the run but leaves its last space for
	// the next token when a non-space follows; \s+ takes the rest.
	end := run(0, isS)
	if end < len(s) {
		if next, _ := utf8.DecodeRuneInString(s[end:]); isN(next) {
			return end // the digit split ends the piece here
		}
	}
	if end < len(s) && end-size > 0 {
		_, last := utf8.DecodeLastRuneInString(s[:end])
		return end - last
	}
	return end
}

// sortedSpecial is for tests: special tokens by id.
func (t *Tokenizer) sortedSpecial() []string {
	var out []string
	for s := range t.special {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return t.special[out[i]] < t.special[out[j]] })
	return out
}
