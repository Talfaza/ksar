package llm

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"
)

func testModel(t *testing.T) (*Model, *Tokenizer, string) {
	dir := os.Getenv("KSAR_LLM_TESTDATA")
	if dir == "" {
		t.Skip("KSAR_LLM_TESTDATA not set")
	}
	tok, err := LoadTokenizer(filepath.Join(dir, "smol", "tokenizer.json"))
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	m, err := Load(filepath.Join(dir, "smol", "model.safetensors"), SmolLM2_135M, 1024)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("loaded in %v", time.Since(start))
	return m, tok, dir
}

// The quantized Go model agrees with an independent float32 numpy
// implementation (ref_logits.json) on the next token and its runners-up.
func TestMatchesReference(t *testing.T) {
	m, _, dir := testModel(t)
	var ref struct {
		IDs    []int     `json:"ids"`
		Logits []float32 `json:"logits"`
	}
	b, err := os.ReadFile(filepath.Join(dir, "ref_logits.json"))
	if err != nil {
		t.Skip("no ref_logits.json")
	}
	json.Unmarshal(b, &ref)
	got, err := m.Logits(ref.IDs)
	if err != nil {
		t.Fatal(err)
	}
	top := func(l []float32) []int {
		idx := make([]int, len(l))
		for i := range idx {
			idx[i] = i
		}
		sort.Slice(idx, func(a, b int) bool { return l[idx[a]] > l[idx[b]] })
		return idx[:5]
	}
	gt, rt := top(got), top(ref.Logits)
	t.Logf("go top5 %v, reference %v", gt, rt)
	if gt[0] != rt[0] {
		t.Errorf("top token %d, reference %d", gt[0], rt[0])
	}
	var maxDiff float64
	for _, i := range rt {
		maxDiff = math.Max(maxDiff, math.Abs(float64(got[i]-ref.Logits[i])))
	}
	if maxDiff > 1.0 {
		t.Errorf("logits differ by up to %.2f from the reference", maxDiff)
	}
	t.Logf("max logit difference on the reference top 5: %.3f", maxDiff)
}

func TestGenerate(t *testing.T) {
	m, tok, _ := testModel(t)
	prompt := tok.Encode("<|im_start|>user\nWhat is the capital of France?<|im_end|>\n<|im_start|>assistant\n")
	start := time.Now()
	out, err := m.Generate(prompt, 40, []int{tok.Special("<|im_end|>")}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("%d prompt + %d new tokens in %v: %q", len(prompt), len(out), time.Since(start), tok.Decode(out))
}
