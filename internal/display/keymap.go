package display

// X11 keysyms (what VNC clients send) to Linux key codes (what the guest's
// virtio keyboard reports), for a US layout. A keysym names a character,
// not a key, so shifted symbols map to the key that types them; the client
// sends Shift separately.

var keysymToKey = func() map[uint32]uint16 {
	m := map[uint32]uint16{
		0xff08: 14, 0xff09: 15, 0xff0d: 28, 0xff1b: 1, 0xffff: 111, // BackSpace Tab Return Escape Delete
		0xff50: 102, 0xff51: 105, 0xff52: 103, 0xff53: 106, 0xff54: 108, // Home Left Up Right Down
		0xff55: 104, 0xff56: 109, 0xff57: 107, 0xff63: 110, 0xff67: 127, // PageUp PageDown End Insert Menu
		0xff13: 119, 0xff14: 70, 0xff61: 99, 0xff7f: 69, // Pause ScrollLock Print NumLock
		0xffe1: 42, 0xffe2: 54, 0xffe3: 29, 0xffe4: 97, 0xffe5: 58, // Shift Ctrl CapsLock
		0xffe7: 125, 0xffe8: 126, 0xffe9: 56, 0xffea: 100, // Meta Alt
		0xffeb: 125, 0xffec: 126, 0xfe03: 100, // Super, AltGr
		0xff8d: 96, 0xffaa: 55, 0xffab: 78, 0xffad: 74, 0xffae: 83, 0xffaf: 98, // keypad
		0xffb7: 71, 0xffb8: 72, 0xffb9: 73, 0xffb4: 75, 0xffb5: 76, 0xffb6: 77,
		0xffb1: 79, 0xffb2: 80, 0xffb3: 81, 0xffb0: 82,
		' ': 57, '-': 12, '_': 12, '=': 13, '+': 13, '[': 26, '{': 26, ']': 27, '}': 27,
		';': 39, ':': 39, '\'': 40, '"': 40, '`': 41, '~': 41, '\\': 43, '|': 43,
		',': 51, '<': 51, '.': 52, '>': 52, '/': 53, '?': 53,
		'!': 2, '@': 3, '#': 4, '$': 5, '%': 6, '^': 7, '&': 8, '*': 9, '(': 10, ')': 11,
	}
	for i, k := range []uint16{11, 2, 3, 4, 5, 6, 7, 8, 9, 10} { // 0-9
		m[uint32('0'+i)] = k
	}
	letters := []uint16{30, 48, 46, 32, 18, 33, 34, 35, 23, 36, 37, 38, 50, 49, 24, 25, 16, 19, 31, 20, 22, 47, 17, 45, 21, 44}
	for i, k := range letters {
		m[uint32('a'+i)] = k
		m[uint32('A'+i)] = k
	}
	for i := uint32(0); i < 10; i++ { // F1-F10
		m[0xffbe+i] = uint16(59 + i)
	}
	m[0xffc8], m[0xffc9] = 87, 88 // F11 F12
	return m
}()

// KeyCode returns the Linux key code for an X keysym, or 0 if unknown.
func KeyCode(keysym uint32) uint16 { return keysymToKey[keysym] }
