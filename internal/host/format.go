package host

import (
	"fmt"
	"io"
	"strings"
)

var marks = map[Status]string{StatusOK: "✓", StatusWarn: "!", StatusFail: "✗"}

// FormatDoctor writes the checks for a terminal: one line for each check
// with a status mark, the fix and its command below a check that is not ok,
// and a summary.
func FormatDoctor(w io.Writer, checks []Check) error {
	width := 0
	for _, c := range checks {
		width = max(width, len(c.Name))
	}
	var b strings.Builder
	count := map[Status]int{}
	for _, c := range checks {
		count[c.Status]++
		fmt.Fprintf(&b, "%s %-*s  %s\n", marks[c.Status], width, c.Name, c.Detail)
		if c.Status != StatusOK && c.Fix != "" {
			fmt.Fprintf(&b, "  %-*s  Fix: %s\n", width, "", c.Fix)
		}
		if c.Status != StatusOK && c.Command != "" {
			fmt.Fprintf(&b, "  %-*s  Run: %s\n", width, "", c.Command)
		}
	}
	if count[StatusFail]+count[StatusWarn] == 0 {
		b.WriteString("\nAll checks passed.\n")
	} else {
		fmt.Fprintf(&b, "\n%s, %s.\n", plural(count[StatusFail], "failure"), plural(count[StatusWarn], "warning"))
	}
	_, err := io.WriteString(w, b.String())
	return err
}

// Worst returns the most severe status of the checks.
func Worst(checks []Check) Status {
	worst := StatusOK
	for _, c := range checks {
		if c.Status == StatusFail {
			return StatusFail
		}
		if c.Status == StatusWarn {
			worst = StatusWarn
		}
	}
	return worst
}

func plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return fmt.Sprintf("%d %ss", n, word)
}
