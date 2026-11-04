package control

import (
	"bufio"
	"errors"
	"io"
	"strings"
)

// ReadLines calls fn for each line of r, without the line end, until r ends.
// It splits lines longer than MaxLine.
func ReadLines(r io.Reader, fn func(line string)) error {
	return readLines(r, MaxLine, fn)
}

func readLines(r io.Reader, max int, fn func(string)) error {
	br := bufio.NewReaderSize(r, max)
	for {
		b, err := br.ReadSlice('\n')
		if len(b) > 0 {
			fn(strings.TrimSuffix(strings.TrimSuffix(string(b), "\n"), "\r"))
		}
		switch {
		case err == nil, errors.Is(err, bufio.ErrBufferFull):
		case errors.Is(err, io.EOF):
			return nil
		default:
			return err
		}
	}
}
