// Package llm runs a tiny language model (SmolLM2-135M-Instruct, a
// Llama-style transformer) on the CPU in pure Go, to suggest how to run a
// project. No GPU, no cgo, no external runtime: it runs wherever ksar
// does. Weights are 8-bit quantized when loaded (about 140 MB in memory).
package llm

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"sort"
)

// tensorInfo is one entry of a safetensors header.
type tensorInfo struct {
	DType   string   `json:"dtype"`
	Shape   []int    `json:"shape"`
	Offsets [2]int64 `json:"data_offsets"`
}

// safetensors is an open .safetensors file.
type safetensors struct {
	f       *os.File
	base    int64 // where tensor data starts
	tensors map[string]tensorInfo
}

func openSafetensors(path string) (*safetensors, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	var n uint64
	if err := binary.Read(f, binary.LittleEndian, &n); err != nil {
		f.Close()
		return nil, err
	}
	if n > 16<<20 {
		f.Close()
		return nil, errors.New("safetensors header too large")
	}
	hdr := make([]byte, n)
	if _, err := io.ReadFull(f, hdr); err != nil {
		f.Close()
		return nil, err
	}
	raw := map[string]json.RawMessage{}
	if err := json.Unmarshal(hdr, &raw); err != nil {
		f.Close()
		return nil, fmt.Errorf("safetensors header: %w", err)
	}
	st := &safetensors{f: f, base: 8 + int64(n), tensors: map[string]tensorInfo{}}
	for name, v := range raw {
		if name == "__metadata__" {
			continue
		}
		var ti tensorInfo
		if err := json.Unmarshal(v, &ti); err != nil {
			f.Close()
			return nil, fmt.Errorf("tensor %s: %w", name, err)
		}
		st.tensors[name] = ti
	}
	return st, nil
}

func (s *safetensors) Close() error { return s.f.Close() }

func (s *safetensors) names() []string {
	var out []string
	for n := range s.tensors {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// float32s reads a bf16 or f32 tensor with the expected shape.
func (s *safetensors) float32s(name string, shape ...int) ([]float32, error) {
	ti, ok := s.tensors[name]
	if !ok {
		return nil, fmt.Errorf("model has no tensor %s", name)
	}
	if fmt.Sprint(ti.Shape) != fmt.Sprint(shape) {
		return nil, fmt.Errorf("tensor %s has shape %v, want %v", name, ti.Shape, shape)
	}
	n := 1
	for _, d := range shape {
		n *= d
	}
	var size int
	switch ti.DType {
	case "BF16", "F16":
		size = 2
	case "F32":
		size = 4
	default:
		return nil, fmt.Errorf("tensor %s: unsupported dtype %s", name, ti.DType)
	}
	if ti.Offsets[1]-ti.Offsets[0] != int64(n*size) {
		return nil, fmt.Errorf("tensor %s: size doesn't match its shape", name)
	}
	buf := make([]byte, n*size)
	if _, err := s.f.ReadAt(buf, s.base+ti.Offsets[0]); err != nil {
		return nil, fmt.Errorf("tensor %s: %w", name, err)
	}
	out := make([]float32, n)
	switch ti.DType {
	case "BF16":
		for i := range out {
			out[i] = math.Float32frombits(uint32(binary.LittleEndian.Uint16(buf[2*i:])) << 16)
		}
	case "F16":
		for i := range out {
			out[i] = f16(binary.LittleEndian.Uint16(buf[2*i:]))
		}
	case "F32":
		for i := range out {
			out[i] = math.Float32frombits(binary.LittleEndian.Uint32(buf[4*i:]))
		}
	}
	return out, nil
}

// f16 converts an IEEE half float.
func f16(h uint16) float32 {
	sign := uint32(h>>15) << 31
	exp := uint32(h>>10) & 0x1f
	frac := uint32(h) & 0x3ff
	switch {
	case exp == 0 && frac == 0:
		return math.Float32frombits(sign)
	case exp == 0: // subnormal
		for frac&0x400 == 0 {
			frac <<= 1
			exp--
		}
		exp++
		frac &= 0x3ff
	case exp == 31:
		return math.Float32frombits(sign | 0x7f800000 | frac<<13)
	}
	return math.Float32frombits(sign | (exp+112)<<23 | frac<<13)
}
