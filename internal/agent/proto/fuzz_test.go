package proto

import (
	"bytes"
	"testing"
)

// FuzzReadFrame parses arbitrary guest output as frames, then decodes
// every JSON message type the host accepts.
func FuzzReadFrame(f *testing.F) {
	var seed bytes.Buffer
	w := NewWriter(&seed)
	w.WriteJSON(TypeReady, Ready{Version: 1})
	w.Write(TypeStdout, []byte("hi"))
	w.WriteJSON(TypeExit, Exit{Code: 1})
	w.Credit(100)
	f.Add(seed.Bytes())
	f.Add([]byte{TypeStdout, 0xff, 0xff, 0xff, 0xff})

	f.Fuzz(func(t *testing.T, data []byte) {
		r := bytes.NewReader(data)
		for {
			fr, err := ReadFrame(r, MaxGuestFrame)
			if err != nil {
				return
			}
			if len(fr.Payload) > MaxGuestFrame {
				t.Fatalf("frame of %d bytes accepted", len(fr.Payload))
			}
			switch fr.Type {
			case TypeReady:
				var v Ready
				Decode(fr, &v)
			case TypeExit:
				var v Exit
				Decode(fr, &v)
			case TypeResult:
				var v Result
				Decode(fr, &v)
			case TypeCredit:
				ParseCredit(fr)
			}
		}
	})
}
