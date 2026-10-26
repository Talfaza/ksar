package llm

import (
	"fmt"
	"math"
	"runtime"
	"sync"
	"sync/atomic"
)

// Config is a Llama-architecture model's shape.
type Config struct {
	Dim, Hidden, Layers, Heads, KVHeads, Vocab int
	RopeTheta, NormEps                         float64
}

// SmolLM2_135M is HuggingFaceTB/SmolLM2-135M-Instruct.
var SmolLM2_135M = Config{Dim: 576, Hidden: 1536, Layers: 30, Heads: 9, KVHeads: 3, Vocab: 49152,
	RopeTheta: 100000, NormEps: 1e-5}

type layer struct {
	attnNorm, ffnNorm []float32
	wq, wk, wv, wo    *q8Matrix
	gate, up, down    *q8Matrix
}

// Model is a loaded, quantized model. It is not safe for concurrent use.
type Model struct {
	cfg              Config
	embed            *q8Matrix // also the output projection (tied)
	layers           []layer
	norm             []float32
	ropeCos, ropeSin []float32 // [maxSeq][headDim/2]
	maxSeq           int
}

// Load reads a .safetensors file and quantizes it.
func Load(path string, cfg Config, maxSeq int) (*Model, error) {
	st, err := openSafetensors(path)
	if err != nil {
		return nil, err
	}
	defer st.Close()
	headDim := cfg.Dim / cfg.Heads
	kvDim := headDim * cfg.KVHeads
	mat := func(name string, rows, cols int) (*q8Matrix, error) {
		w, err := st.float32s(name, rows, cols)
		if err != nil {
			return nil, err
		}
		return quantize(w, rows, cols), nil
	}
	m := &Model{cfg: cfg, maxSeq: maxSeq}
	if m.embed, err = mat("model.embed_tokens.weight", cfg.Vocab, cfg.Dim); err != nil {
		return nil, err
	}
	if m.norm, err = st.float32s("model.norm.weight", cfg.Dim); err != nil {
		return nil, err
	}
	for i := 0; i < cfg.Layers; i++ {
		p := fmt.Sprintf("model.layers.%d.", i)
		var l layer
		steps := []struct {
			dst        **q8Matrix
			name       string
			rows, cols int
		}{
			{&l.wq, "self_attn.q_proj.weight", cfg.Dim, cfg.Dim},
			{&l.wk, "self_attn.k_proj.weight", kvDim, cfg.Dim},
			{&l.wv, "self_attn.v_proj.weight", kvDim, cfg.Dim},
			{&l.wo, "self_attn.o_proj.weight", cfg.Dim, cfg.Dim},
			{&l.gate, "mlp.gate_proj.weight", cfg.Hidden, cfg.Dim},
			{&l.up, "mlp.up_proj.weight", cfg.Hidden, cfg.Dim},
			{&l.down, "mlp.down_proj.weight", cfg.Dim, cfg.Hidden},
		}
		for _, s := range steps {
			if *s.dst, err = mat(p+s.name, s.rows, s.cols); err != nil {
				return nil, err
			}
		}
		if l.attnNorm, err = st.float32s(p+"input_layernorm.weight", cfg.Dim); err != nil {
			return nil, err
		}
		if l.ffnNorm, err = st.float32s(p+"post_attention_layernorm.weight", cfg.Dim); err != nil {
			return nil, err
		}
		m.layers = append(m.layers, l)
	}
	// RoPE tables (Hugging Face's non-interleaved layout: dimension i pairs
	// with i + headDim/2).
	half := headDim / 2
	m.ropeCos = make([]float32, maxSeq*half)
	m.ropeSin = make([]float32, maxSeq*half)
	for pos := 0; pos < maxSeq; pos++ {
		for i := 0; i < half; i++ {
			freq := math.Pow(cfg.RopeTheta, -float64(2*i)/float64(headDim))
			a := float64(pos) * freq
			m.ropeCos[pos*half+i] = float32(math.Cos(a))
			m.ropeSin[pos*half+i] = float32(math.Sin(a))
		}
	}
	return m, nil
}

// state is a sequence's KV cache.
type state struct {
	m              *Model
	pos            int
	kCache, vCache [][]float32 // per layer: [maxSeq][kvDim]
	logits         []float32   // after the last feed, for its last token
	capacity       int         // tokens the cache has room for
}

func (m *Model) newState() *state { return m.newStateCap(m.maxSeq) }

// newStateCap makes a state with room for capacity tokens.
func (m *Model) newStateCap(capacity int) *state {
	c := m.cfg
	kvDim := c.Dim / c.Heads * c.KVHeads
	s := &state{m: m, logits: make([]float32, c.Vocab), capacity: capacity}
	for range c.Layers {
		s.kCache = append(s.kCache, make([]float32, capacity*kvDim))
		s.vCache = append(s.vCache, make([]float32, capacity*kvDim))
	}
	return s
}

func rmsnorm(out, x, w []float32, eps float64) {
	var ss float64
	for _, v := range x {
		ss += float64(v) * float64(v)
	}
	inv := float32(1 / math.Sqrt(ss/float64(len(x))+eps))
	for i, v := range x {
		out[i] = v * inv * w[i]
	}
}

func (s *state) rope(vec []float32, heads, pos int) {
	headDim := s.m.cfg.Dim / s.m.cfg.Heads
	half := headDim / 2
	cos := s.m.ropeCos[pos*half : (pos+1)*half]
	sin := s.m.ropeSin[pos*half : (pos+1)*half]
	for h := 0; h < heads; h++ {
		v := vec[h*headDim : (h+1)*headDim]
		for i := 0; i < half; i++ {
			a, b := v[i], v[i+half]
			v[i] = a*cos[i] - b*sin[i]
			v[i+half] = b*cos[i] + a*sin[i]
		}
	}
}

func matrix(rows, cols int) [][]float32 {
	buf := make([]float32, rows*cols)
	out := make([][]float32, rows)
	for i := range out {
		out[i] = buf[i*cols : (i+1)*cols]
	}
	return out
}

// feed runs a batch of tokens at the next positions. allLogits returns the
// next-token scores after every token (to score a known continuation);
// otherwise only s.logits, after the last one, is set.
func (s *state) feed(tokens []int, allLogits bool) ([][]float32, error) {
	m, c := s.m, s.m.cfg
	T := len(tokens)
	if T == 0 {
		return nil, nil
	}
	if s.pos+T > s.capacity {
		return nil, fmt.Errorf("context longer than %d tokens", s.capacity)
	}
	headDim := c.Dim / c.Heads
	kvDim := headDim * c.KVHeads
	group := c.Heads / c.KVHeads
	scale := float32(1 / math.Sqrt(float64(headDim)))
	x, h := matrix(T, c.Dim), matrix(T, c.Dim)
	q, att := matrix(T, c.Dim), matrix(T, c.Dim)
	k, v := matrix(T, kvDim), matrix(T, kvDim)
	ga, gb := matrix(T, c.Hidden), matrix(T, c.Hidden)
	for t, tok := range tokens {
		if tok < 0 || tok >= c.Vocab {
			return nil, fmt.Errorf("token %d out of range", tok)
		}
		m.embed.row(tok, x[t])
	}

	for li := range m.layers {
		l := &m.layers[li]
		for t := range x {
			rmsnorm(h[t], x[t], l.attnNorm, c.NormEps)
		}
		l.wq.matMul(q, h)
		l.wk.matMul(k, h)
		l.wv.matMul(v, h)
		kc, vc := s.kCache[li], s.vCache[li]
		for t := range x {
			s.rope(q[t], c.Heads, s.pos+t)
			s.rope(k[t], c.KVHeads, s.pos+t)
			copy(kc[(s.pos+t)*kvDim:], k[t])
			copy(vc[(s.pos+t)*kvDim:], v[t])
		}
		// Attention, causal: token t sees positions up to its own. Tokens
		// are independent here, so they are spread across CPUs.
		parallel(T, func(t int, scores []float32) {
			n := s.pos + t + 1
			for hd := 0; hd < c.Heads; hd++ {
				qh := q[t][hd*headDim : (hd+1)*headDim]
				kvOff := (hd / group) * headDim
				sc := scores[:n]
				mx := float32(math.Inf(-1))
				for p := 0; p < n; p++ {
					sc[p] = dotF32(qh, kc[p*kvDim+kvOff:p*kvDim+kvOff+headDim]) * scale
					if sc[p] > mx {
						mx = sc[p]
					}
				}
				var sum float32
				for p := range sc {
					sc[p] = float32(math.Exp(float64(sc[p] - mx)))
					sum += sc[p]
				}
				out := att[t][hd*headDim : (hd+1)*headDim]
				clear(out)
				for p, w := range sc {
					w /= sum
					vv := vc[p*kvDim+kvOff : p*kvDim+kvOff+headDim]
					for i := range out {
						out[i] += w * vv[i]
					}
				}
			}
		}, s.pos+T)
		l.wo.matMul(h, att)
		for t := range x {
			for i := range x[t] {
				x[t][i] += h[t][i]
			}
			rmsnorm(h[t], x[t], l.ffnNorm, c.NormEps)
		}
		l.gate.matMul(ga, h)
		l.up.matMul(gb, h)
		for t := range ga {
			for i, g := range ga[t] {
				ga[t][i] = g / (1 + float32(math.Exp(float64(-g)))) * gb[t][i] // SiLU(gate) * up
			}
		}
		l.down.matMul(h, ga)
		for t := range x {
			for i := range x[t] {
				x[t][i] += h[t][i]
			}
		}
	}
	s.pos += T
	if !allLogits {
		rmsnorm(h[T-1], x[T-1], m.norm, c.NormEps)
		m.embed.matMul([][]float32{s.logits}, [][]float32{h[T-1]})
		return nil, nil
	}
	logits := matrix(T, c.Vocab)
	for t := range x {
		rmsnorm(h[t], x[t], m.norm, c.NormEps)
	}
	m.embed.matMul(logits, h)
	copy(s.logits, logits[T-1])
	return logits, nil
}

// parallel runs f(i) for i in [0, n) on all CPUs, giving each worker its
// own scratch buffer of the given size.
func parallel(n int, f func(i int, scratch []float32), scratch int) {
	workers := min(runtime.GOMAXPROCS(0), n)
	var next atomic.Int64
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			buf := make([]float32, scratch)
			for {
				i := int(next.Add(1) - 1)
				if i >= n {
					return
				}
				f(i, buf)
			}
		}()
	}
	wg.Wait()
}

// step feeds one token.
func (s *state) step(token int, wantLogits bool) error {
	_, err := s.feed([]int{token}, false)
	return err
}

// Generate continues prompt greedily (always the most likely token) until
// a stop token, stop(text so far) returns true, or maxNew tokens.
func (m *Model) Generate(prompt []int, maxNew int, stopTokens []int, stop func(ids []int) bool) ([]int, error) {
	if len(prompt) == 0 {
		return nil, fmt.Errorf("empty prompt")
	}
	if len(prompt)+maxNew > m.maxSeq {
		return nil, fmt.Errorf("prompt of %d tokens is too long", len(prompt))
	}
	s := m.newState()
	if _, err := s.feed(prompt, false); err != nil {
		return nil, err
	}
	var out []int
	for len(out) < maxNew {
		best := 0
		for i, v := range s.logits {
			if v > s.logits[best] {
				best = i
			}
		}
		for _, st := range stopTokens {
			if best == st {
				return out, nil
			}
		}
		out = append(out, best)
		if stop != nil && stop(out) {
			return out, nil
		}
		if err := s.step(best, true); err != nil {
			return out, err
		}
	}
	return out, nil
}

// Logits runs a prompt and returns the next-token scores (for tests).
func (m *Model) Logits(prompt []int) ([]float32, error) {
	s := m.newState()
	if _, err := s.feed(prompt, false); err != nil {
		return nil, err
	}
	return append([]float32(nil), s.logits...), nil
}
