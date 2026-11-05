package guest

import (
	"bufio"
	"errors"
	"io"
	"strconv"
	"strings"
	"time"
)

// LogLine is one line of sandbox output.
type LogLine struct {
	Time   time.Time
	Source string
	Text   string
}

// Sources of log lines.
const (
	SourceRunner  = "runner"  // the output of the runner
	SourceDiag    = "diag"    // the step output of the job, in the GitHub log format
	SourceInit    = "init"    // the log of runplane-init
	SourceDockerd = "dockerd" // the output of dockerd
	SourceConsole = "console" // the serial console of a VM, which the host reads
)

// MaxLine is the longest text of one log line. Longer texts are split.
const MaxLine = 64 << 10

const (
	maxSource = 16
	// maxWireLine is the longest line of the log stream:
	// "<unix-ms> <source> <text>\n".
	maxWireLine = len("-9223372036854775808") + 1 + maxSource + 1 + MaxLine + 1
)

// AppendLogLine appends l to b in the form of the log stream,
// "<unix-ms> <source> <text>\n". A text longer than MaxLine goes in more
// lines, and a source longer than 16 bytes is cut, so that the host reads
// each line in one piece.
func AppendLogLine(b []byte, l LogLine) []byte {
	source := l.Source[:min(len(l.Source), maxSource)]
	text := l.Text
	for {
		part := text[:min(len(text), MaxLine)]
		b = strconv.AppendInt(b, l.Time.UnixMilli(), 10)
		b = append(b, ' ')
		b = append(b, source...)
		b = append(b, ' ')
		b = append(b, part...)
		b = append(b, '\n')
		text = text[len(part):]
		if text == "" {
			return b
		}
	}
}

// ReadLogLines calls fn for each line of the log stream r until r ends.
func ReadLogLines(r io.Reader, fn func(LogLine)) error {
	return readLines(r, maxWireLine, func(line string) { fn(parseLogLine(line)) })
}

// parseLogLine reads a line of the log stream. It keeps a line in another
// form as text with no source.
func parseLogLine(line string) LogLine {
	ms, rest, ok := strings.Cut(line, " ")
	source, text, _ := strings.Cut(rest, " ")
	n, err := strconv.ParseInt(ms, 10, 64)
	if !ok || err != nil || source == "" {
		return LogLine{Time: time.Now().UTC(), Text: line}
	}
	return LogLine{Time: time.UnixMilli(n).UTC(), Source: source, Text: text}
}

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
