package tui

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// clean makes untrusted text (log lines, addresses, subjects, remote servers' answers) safe to print to a
// terminal. Escape sequences in it could otherwise retitle the window, write the clipboard (OSC 52) or redraw
// the screen, and bidi controls could reorder what's shown. Those become visible escapes; bad UTF-8 becomes U+FFFD.
func clean(s string) string {
	if safe(s) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		i += size
		switch {
		case r == utf8.RuneError && size == 1:
			b.WriteRune('�')
		case r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0):
			fmt.Fprintf(&b, `\x%02x`, r)
		case bidi(r):
			fmt.Fprintf(&b, `\u%04x`, r)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

func safe(s string) bool {
	for i := 0; i < len(s); i++ {
		if c := s[i]; c < 0x20 || c >= 0x7f {
			return false
		}
	}
	return true
}

func bidi(r rune) bool {
	return r == 0x061c || r == 0x200e || r == 0x200f || (r >= 0x202a && r <= 0x202e) || (r >= 0x2066 && r <= 0x2069)
}
