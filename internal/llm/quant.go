package llm

import (
	"math"
	"runtime"
	"sync"
)

// qBlock is the number of weights sharing one scale (as llama.cpp's Q8_0).
const qBlock = 32

// q8Matrix is a rows x cols weight matrix quantized to int8 in blocks of
// 32 along each row, each block with its own scale.
type q8Matrix struct {
	rows, cols int
	q          []int8
	scale      []float32 // rows * cols/qBlock
}

func quantize(w []float32, rows, cols int) *q8Matrix {
	m := &q8Matrix{rows: rows, cols: cols, q: make([]int8, rows*cols), scale: make([]float32, rows*cols/qBlock)}
	for b := 0; b < len(m.scale); b++ {
		blk := w[b*qBlock : (b+1)*qBlock]
		var max float32
		for _, v := range blk {
			if a := float32(math.Abs(float64(v))); a > max {
				max = a
			}
		}
		s := max / 127
		m.scale[b] = s
		if s == 0 {
			continue
		}
		for i, v := range blk {
			m.q[b*qBlock+i] = int8(math.Round(float64(v / s)))
		}
	}
	return m
}

// row dequantizes one row into out (for embedding lookups).
func (m *q8Matrix) row(r int, out []float32) {
	base := r * m.cols
	q := m.q[base : base+m.cols]
	for b, s := range m.scale[base/qBlock : (base+m.cols)/qBlock] {
		qb, ob := q[b*qBlock:(b+1)*qBlock], out[b*qBlock:(b+1)*qBlock]
		for i := range qb {
			ob[i] = float32(qb[i]) * s
		}
	}
}

// dot is one row times x.
func (m *q8Matrix) dot(r int, x []float32) float32 {
	if len(x) != m.cols {
		panic("llm: dimension mismatch")
	}
	base := r * m.cols
	q := m.q[base : base+m.cols]
	scales := m.scale[base/qBlock : (base+m.cols)/qBlock]
	if useAVX2 {
		return dotQ8AVX2(&q[0], &scales[0], &x[0], len(scales))
	}
	var sum float32
	for b, s := range scales {
		var acc float32
		qb, xb := q[b*qBlock:(b+1)*qBlock], x[b*qBlock:(b+1)*qBlock]
		for i := range qb {
			acc += float32(qb[i]) * xb[i]
		}
		sum += acc * s
	}
	return sum
}

// matVec computes out = M x, splitting rows across CPUs.
func (m *q8Matrix) matVec(out, x []float32) {
	workers := runtime.GOMAXPROCS(0)
	if m.rows < 256 || workers == 1 {
		for r := 0; r < m.rows; r++ {
			out[r] = m.dot(r, x)
		}
		return
	}
	chunk := (m.rows + workers - 1) / workers
	var wg sync.WaitGroup
	for start := 0; start < m.rows; start += chunk {
		end := min(start+chunk, m.rows)
		wg.Add(1)
		go func(start, end int) {
			defer wg.Done()
			for r := start; r < end; r++ {
				out[r] = m.dot(r, x)
			}
		}(start, end)
	}
	wg.Wait()
}

// dotF32 is a plain float dot product (len a multiple of 32 for AVX2).
func dotF32(a, b []float32) float32 {
	if useAVX2 && len(a)%32 == 0 && len(a) > 0 {
		return dotF32AVX2(&a[0], &b[0], len(a))
	}
	var s float32
	for i := range a {
		s += a[i] * b[i]
	}
	return s
}

// matMul computes out[t] = M x[t] for a batch of vectors. Each worker
// dequantizes a row once and reuses it for every vector, so a batch costs
// little more memory traffic than one vector.
func (m *q8Matrix) matMul(out, x [][]float32) {
	if len(x) == 1 { // one vector: the int8 kernel, no dequantizing
		m.matVec(out[0], x[0])
		return
	}
	for _, v := range x {
		if len(v) != m.cols {
			panic("llm: dimension mismatch")
		}
	}
	workers := runtime.GOMAXPROCS(0)
	chunk := max((m.rows+workers-1)/workers, 16)
	var wg sync.WaitGroup
	for start := 0; start < m.rows; start += chunk {
		end := min(start+chunk, m.rows)
		wg.Add(1)
		go func(start, end int) {
			defer wg.Done()
			row := make([]float32, m.cols)
			for r := start; r < end; r++ {
				m.row(r, row)
				for t := range x {
					out[t][r] = dotF32(row, x[t])
				}
			}
		}(start, end)
	}
	wg.Wait()
}
