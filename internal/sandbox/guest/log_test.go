package guest

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

// The host must read each line of the log stream in one piece, whatever the
// source name and the text length are.
func TestLogStreamKeepsLinesWhole(t *testing.T) {
	at := time.UnixMilli(1793786400123).UTC()
	full := strings.Repeat("x", MaxLine)
	cases := []struct {
		name string
		in   LogLine
		want []LogLine
	}{
		{"empty text", LogLine{Time: at, Source: "dockerd"}, []LogLine{{Time: at, Source: "dockerd"}}},
		{"longest text", LogLine{Time: at, Source: "dockerd", Text: full}, []LogLine{{Time: at, Source: "dockerd", Text: full}}},
		{
			"longer text",
			LogLine{Time: at, Source: "diag", Text: full + "yz"},
			[]LogLine{{Time: at, Source: "diag", Text: full}, {Time: at, Source: "diag", Text: "yz"}},
		},
		{
			"long source",
			LogLine{Time: at, Source: "a-source-of-forty-bytes-and-more-than-it", Text: full},
			[]LogLine{{Time: at, Source: "a-source-of-fort", Text: full}},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var got []LogLine
			err := ReadLogLines(bytes.NewReader(AppendLogLine(nil, c.in)), func(l LogLine) { got = append(got, l) })
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != len(c.want) {
				t.Fatalf("got %d lines, want %d", len(got), len(c.want))
			}
			for i, l := range got {
				if l != c.want[i] {
					t.Errorf("line %d: %s %q %d bytes, want %s %q %d bytes",
						i, l.Time, l.Source, len(l.Text), c.want[i].Time, c.want[i].Source, len(c.want[i].Text))
				}
			}
		})
	}
}
