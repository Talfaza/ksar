// Package guest holds tiny hand-assembled guest programs used to exercise
// the VMM before it can boot Linux.
package guest

// LoadAddr is where the real-mode programs below expect to be loaded
// (CS base 0, IP 0x1000).
const LoadAddr = 0x1000

// SerialPort is the COM1 data port the programs write text to.
const SerialPort = 0x3F8

// ExitPort is a Ksar-specific "debug exit" port: writing a byte to it ends
// the run with that byte as the exit code.
const ExitPort = 0xF4

// Hello prints a message to the serial port, one byte at a time, then
// reports exit code 42 on the exit port.
//
//	org 0x1000
//	        mov  si, msg        ; BE 13 10
//	        mov  dx, 0x3f8      ; BA F8 03
//	.loop:  lodsb               ; AC          al = [ds:si], si++
//	        test al, al         ; 84 C0
//	        jz   .done          ; 74 03
//	        out  dx, al         ; EE          -> KVM_EXIT_IO
//	        jmp  .loop          ; EB F8
//	.done:  mov  al, 42         ; B0 2A
//	        out  0xf4, al       ; E6 F4       -> exit code
//	        hlt                 ; F4
//	msg:    db "Hello from inside a Ksar microVM!", 10, 0
var Hello = append([]byte{
	0xBE, 0x13, 0x10, // mov si, 0x1013 (msg)
	0xBA, 0xF8, 0x03, // mov dx, 0x3f8
	0xAC,       // lodsb
	0x84, 0xC0, // test al, al
	0x74, 0x03, // jz +3 -> .done
	0xEE,       // out dx, al
	0xEB, 0xF8, // jmp -8 -> .loop
	0xB0, 0x2A, // mov al, 42
	0xE6, 0xF4, // out 0xf4, al
	0xF4, // hlt
}, []byte("Hello from inside a Ksar microVM!\n\x00")...)

// HelloExitCode is the exit code Hello reports.
const HelloExitCode = 42

// Spin loops forever. It is used to check that timeouts really stop a guest.
//
//	org 0x1000
//	.l: jmp .l          ; EB FE
var Spin = []byte{0xEB, 0xFE}
