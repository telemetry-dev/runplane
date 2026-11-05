// Package field holds the values of a change that runplane refuses, each at
// the JSON key of its field. The services return them, and the API shows
// each one at its field of the form.
package field

import (
	"strings"
	"unicode"
)

// Problem is a value that a change cannot have.
type Problem struct {
	Key     string // for example "name", "dns_servers[1]", or "backup_s3.bucket"
	Message string // a sentence for the user
	Value   any
}

// Problems is the error of a change with one refused value or more.
type Problems []Problem

func (p *Problems) Add(key, msg string, value any) {
	*p = append(*p, Problem{Key: key, Message: msg, Value: value})
}

// Err returns p, or nil without problems.
func (p Problems) Err() error {
	if len(p) == 0 {
		return nil
	}
	return p
}

func (p Problems) Error() string {
	msgs := make([]string, len(p))
	for i, x := range p {
		msgs[i] = x.Key + ": " + x.Message
	}
	return strings.Join(msgs, " ")
}

// Sentence makes the message of an error into a sentence for the user.
func Sentence(err error) string {
	msg := []rune(err.Error())
	if len(msg) == 0 {
		return ""
	}
	msg[0] = unicode.ToUpper(msg[0])
	if msg[len(msg)-1] != '.' {
		msg = append(msg, '.')
	}
	return string(msg)
}
