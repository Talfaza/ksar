package llm

import (
	"fmt"
	"math"
	"strings"
	"sync"
)

// Assistant picks the most plausible command for a project from a list
// of candidates, by how likely the model finds each as its answer. It
// never writes a command itself, so it can't produce nonsense, and text
// in a project can't make it produce anything outside the list.
type Assistant struct {
	m   *Model
	tok *Tokenizer

	mu     sync.Mutex // the model's scratch state is per call; one call at a time
	prefix *state     // the fixed part of the prompt, computed once
}

// NewAssistant prepares the fixed part of the prompt.
func NewAssistant(m *Model, tok *Tokenizer) (*Assistant, error) {
	a := &Assistant{m: m, tok: tok, prefix: m.newState()}
	if _, err := a.prefix.feed(tok.Encode(promptPrefix()), false); err != nil {
		return nil, err
	}
	return a, nil
}

func promptPrefix() string {
	var b strings.Builder
	b.WriteString("<|im_start|>system\n" + system + "<|im_end|>\n")
	for _, e := range examples {
		b.WriteString("<|im_start|>user\n" + e.q + "<|im_end|>\n<|im_start|>assistant\n" + e.a + "<|im_end|>\n")
	}
	return b.String()
}

// clone copies a state up to its position, with room for extra more
// tokens.
func (s *state) clone(extra int) *state {
	c := s.m.newStateCap(min(s.pos+extra, s.m.maxSeq))
	c.pos = s.pos
	kvDim := s.m.cfg.Dim / s.m.cfg.Heads * s.m.cfg.KVHeads
	for l := range s.kCache {
		copy(c.kCache[l][:s.pos*kvDim], s.kCache[l][:s.pos*kvDim])
		copy(c.vCache[l][:s.pos*kvDim], s.vCache[l][:s.pos*kvDim])
	}
	copy(c.logits, s.logits)
	return c
}

func logSoftmax(logits []float32, tok int) float64 {
	max := logits[0]
	for _, v := range logits {
		if v > max {
			max = v
		}
	}
	var sum float64
	for _, v := range logits {
		sum += math.Exp(float64(v - max))
	}
	return float64(logits[tok]-max) - math.Log(sum)
}

// Rank scores each candidate (higher is more likely): its average
// log-probability per token as the answer, end of answer included.
func (a *Assistant) Rank(f Facts, candidates []string) ([]float64, error) {
	if len(candidates) == 0 {
		return nil, fmt.Errorf("no candidates")
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	q := a.tok.Encode("<|im_start|>user\n" + f.describe() + "<|im_end|>\n<|im_start|>assistant\n")
	if a.prefix.pos+len(q)+64 > a.m.maxSeq {
		return nil, fmt.Errorf("project description too long")
	}
	base := a.prefix.clone(len(q))
	if _, err := base.feed(q, false); err != nil {
		return nil, err
	}
	end := a.tok.Special("<|im_end|>")
	scores := make([]float64, len(candidates))
	for ci, cand := range candidates {
		ids := append(a.tok.Encode(cand), end)
		if base.pos+len(ids) > a.m.maxSeq {
			scores[ci] = math.Inf(-1)
			continue
		}
		// The whole candidate in one batch: the scores after each of its
		// tokens give the probability of the next one.
		s := base.clone(len(ids))
		after, err := s.feed(ids[:len(ids)-1], true)
		if err != nil {
			return nil, err
		}
		lp := logSoftmax(base.logits, ids[0])
		for i := 1; i < len(ids); i++ {
			lp += logSoftmax(after[i-1], ids[i])
		}
		scores[ci] = lp / float64(len(ids))
	}
	return scores, nil
}
