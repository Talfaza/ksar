// Package agentbin embeds the ksar-agent binary into ksar, so a single
// executable carries everything needed to boot a sandbox. The Makefile
// builds the agent into bin/ before building ksar.
package agentbin

import (
	"embed"
	"errors"
)

//go:embed all:bin
var files embed.FS

// Agent returns the embedded static ksar-agent binary.
func Agent() ([]byte, error) {
	b, err := files.ReadFile("bin/ksar-agent")
	if err != nil {
		return nil, errors.New("ksar-agent is not embedded in this build: build with `make`, or pass -agent")
	}
	return b, nil
}
