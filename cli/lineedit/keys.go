package lineedit

import (
	"strings"
	"unicode/utf8"
)

// KeyCode names a non-character key.
type KeyCode int

const (
	KeyRune KeyCode = iota // a printable character, in Key.Rune
	KeyEnter
	KeyBackspace
	KeyDelete
	KeyLeft
	KeyRight
	KeyHome
	KeyEnd
	KeyUp
	KeyDown
	KeyTab
	KeyEscape
	KeyCtrlA
	KeyCtrlB
	KeyCtrlC
	KeyCtrlD
	KeyCtrlE
	KeyCtrlF
	KeyCtrlG
	KeyCtrlK
	KeyCtrlL
	KeyCtrlN
	KeyCtrlP
	KeyCtrlR
	KeyCtrlU
	KeyCtrlW
	KeyUnknown
)

// Key is one key press.
type Key struct {
	Code KeyCode
	Rune rune
}

// R is a character key.
func R(r rune) Key { return Key{Code: KeyRune, Rune: r} }

var csiKeys = map[string]KeyCode{
	"A": KeyUp, "B": KeyDown, "C": KeyRight, "D": KeyLeft, "H": KeyHome, "F": KeyEnd,
	"1~": KeyHome, "7~": KeyHome, "4~": KeyEnd, "8~": KeyEnd, "3~": KeyDelete,
}

var ctrlKeys = map[byte]KeyCode{
	0x01: KeyCtrlA, 0x02: KeyCtrlB, 0x03: KeyCtrlC, 0x04: KeyCtrlD, 0x05: KeyCtrlE, 0x06: KeyCtrlF,
	0x07: KeyCtrlG, 0x08: KeyBackspace, 0x09: KeyTab, 0x0a: KeyEnter, 0x0b: KeyCtrlK, 0x0c: KeyCtrlL,
	0x0d: KeyEnter, 0x0e: KeyCtrlN, 0x10: KeyCtrlP, 0x12: KeyCtrlR, 0x15: KeyCtrlU, 0x17: KeyCtrlW,
	0x7f: KeyBackspace,
}

// ParseKeys turns terminal input (a typed key, an escape sequence, or a
// paste) into keys. Unknown sequences become KeyUnknown, never text.
func ParseKeys(data string) []Key {
	var out []Key
	for i := 0; i < len(data); {
		c := data[i]
		if c == 0x1b {
			// ESC [ … final, or ESC O x (application cursor keys).
			if i+1 < len(data) && (data[i+1] == '[' || data[i+1] == 'O') {
				j := i + 2
				for j < len(data) && (data[j] >= '0' && data[j] <= '9' || data[j] == ';') {
					j++
				}
				if j < len(data) {
					seq := data[i+2 : j+1]
					// Modifiers (ESC [1;5D …) are folded onto the bare key.
					if k := strings.LastIndexByte(seq, ';'); k >= 0 {
						seq = seq[len(seq)-1:]
					}
					if code, ok := csiKeys[seq]; ok {
						out = append(out, Key{Code: code})
					} else {
						out = append(out, Key{Code: KeyUnknown})
					}
					i = j + 1
					continue
				}
			}
			out = append(out, Key{Code: KeyEscape})
			i++
			continue
		}
		if c < 0x20 || c == 0x7f {
			if code, ok := ctrlKeys[c]; ok {
				out = append(out, Key{Code: code})
			} else {
				out = append(out, Key{Code: KeyUnknown})
			}
			i++
			continue
		}
		r, n := utf8.DecodeRuneInString(data[i:])
		if r == utf8.RuneError && n <= 1 || r >= 0x80 && r <= 0x9f {
			out = append(out, Key{Code: KeyUnknown})
		} else {
			out = append(out, R(r))
		}
		i += n
	}
	return out
}
