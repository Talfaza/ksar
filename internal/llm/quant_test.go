package llm

import (
	"math"
	"math/rand/v2"
	"testing"
)

// The AVX2 kernel and the Go loop agree, and quantization stays close to
// the float result.
func TestDotQ8(t *testing.T) {
	rows, cols := 7, 576
	w := make([]float32, rows*cols)
	x := make([]float32, cols)
	for i := range w {
		w[i] = float32(rand.NormFloat64()) * 0.05
	}
	for i := range x {
		x[i] = float32(rand.NormFloat64())
	}
	m := quantize(w, rows, cols)
	for r := 0; r < rows; r++ {
		var exact float64
		for i := 0; i < cols; i++ {
			exact += float64(w[r*cols+i]) * float64(x[i])
		}
		saved := useAVX2
		useAVX2 = false
		goSum := m.dot(r, x)
		useAVX2 = saved
		asmSum := m.dot(r, x)
		if math.Abs(float64(goSum-asmSum)) > 1e-3 {
			t.Errorf("row %d: Go %v, asm %v", r, goSum, asmSum)
		}
		if math.Abs(float64(goSum)-exact) > 0.02*math.Max(1, math.Abs(exact)) {
			t.Errorf("row %d: quantized %v, exact %v", r, goSum, exact)
		}
	}
}
