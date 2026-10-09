// Package mailtext holds what the mail adapters share: keeping a body short
// enough for the model, and checking the addresses an agent writes to.
package mailtext

import (
	"fmt"
	"net/mail"
	"regexp"
	"strings"
)

// MaxRecipients is per field: an agent writing to more is more likely a
// mistake.
const MaxRecipients = 50

var blankLines = regexp.MustCompile(`\n[ \t]*(\n[ \t]*){2,}`)

// Body keeps a body to n characters, and to no more than one blank line in
// a row, as text converted from HTML often has.
func Body(s string, n int) string {
	s = strings.TrimSpace(blankLines.ReplaceAllString(strings.ReplaceAll(s, "\r\n", "\n"), "\n\n"))
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "\n[cut]"
}

// Addresses checks the addresses the model gives in one field, such as
// "to". Each may name the person too, such as "Ada <ada@example.com>".
func Addresses(field string, list []string) ([]*mail.Address, error) {
	if len(list) > MaxRecipients {
		return nil, fmt.Errorf("%s has more than %d addresses", field, MaxRecipients)
	}
	out := make([]*mail.Address, 0, len(list))
	for _, s := range list {
		addr, err := mail.ParseAddress(strings.TrimSpace(s))
		if err != nil {
			return nil, fmt.Errorf("%q in %s is not an email address", s, field)
		}
		out = append(out, addr)
	}
	return out, nil
}
