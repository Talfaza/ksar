package llm

import "golang.org/x/sys/cpu"

// useAVX2 selects the assembly kernel; other CPUs use the Go loop.
var useAVX2 = cpu.X86.HasAVX2 && cpu.X86.HasFMA

//go:noescape
func dotQ8AVX2(q *int8, scales *float32, x *float32, blocks int) float32

//go:noescape
func dotF32AVX2(a, b *float32, n int) float32
