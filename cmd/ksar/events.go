package main

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"os"
	"strings"
	"sync"
	"time"
)

// events writes one JSON object per line to a file descriptor handed in by
// a supervisor (the web UI). It is opened before lockdown; writing to an
// already-open fd stays allowed afterwards.
type events struct {
	mu    sync.Mutex
	f     *os.File
	start time.Time
}

func openEvents(fd int) *events {
	e := &events{start: time.Now()}
	if fd >= 0 {
		e.f = os.NewFile(uintptr(fd), "events")
	}
	return e
}

func (e *events) emit(kind string, fields map[string]any) {
	if e == nil || e.f == nil {
		return
	}
	m := map[string]any{"event": kind, "t_ms": time.Since(e.start).Milliseconds()}
	for k, v := range fields {
		m[k] = v
	}
	b, err := json.Marshal(m)
	if err != nil {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.f.Write(append(b, '\n'))
}

// kernelVersion returns the version string from a bzImage setup header.
func kernelVersion(k []byte) string {
	if len(k) < 0x210 || string(k[0x202:0x206]) != "HdrS" {
		return ""
	}
	off := 0x200 + int(binary.LittleEndian.Uint16(k[0x20e:]))
	if off >= len(k) {
		return ""
	}
	end := bytes.IndexByte(k[off:min(len(k), off+256)], 0)
	if end < 0 {
		return ""
	}
	v, _, _ := strings.Cut(string(k[off:off+end]), " ")
	return v
}
