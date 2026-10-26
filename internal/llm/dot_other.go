//go:build !amd64

package llm

var useAVX2 = false

func dotQ8AVX2(q *int8, scales *float32, x *float32, blocks int) float32 { panic("unreachable") }

func dotF32AVX2(a, b *float32, n int) float32 { panic("unreachable") }
