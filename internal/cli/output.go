package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"golang.org/x/term"
	"sigs.k8s.io/yaml"
)

// Two audiences, and the flag that picks between them.
//
// A panel talks to a person. This talks to a person AND to a program, and the
// second one is half the reason the CLI exists: `--output json` is what makes a
// list something jq can filter and a CI step can branch on.
//
// The two are rendered from different things on purpose. The table is a chosen
// handful of columns, because thirty fields in a terminal is not a table. The
// JSON is the API's OWN body, unshaped, because a script that parses it should
// see what the API said rather than what this CLI decided to keep -- and because
// a re-shaped structure is a second schema that drifts from the first one.

// Format is the --output value.
type Format string

const (
	FormatTable Format = "table"
	FormatJSON  Format = "json"
	FormatYAML  Format = "yaml"
)

// Printer renders a command's result.
type Printer struct {
	Format Format
	Out    io.Writer
	Err    io.Writer
	// Color is off when the output is not a terminal, when --no-color is given,
	// or when NO_COLOR is set. Colour in a pipe is escape codes in somebody's
	// log file.
	Color bool
}

// NewPrinter builds the printer for a run.
func NewPrinter(format Format, noColor bool) *Printer {
	return &Printer{
		Format: format,
		Out:    os.Stdout,
		Err:    os.Stderr,
		Color:  colorAllowed(noColor),
	}
}

func colorAllowed(noColor bool) bool {
	if noColor || os.Getenv("NO_COLOR") != "" {
		return false
	}
	return term.IsTerminal(int(os.Stdout.Fd()))
}

// Table is a rendered listing: the columns a person reads.
type Table struct {
	Headers []string
	Rows    [][]string
	// Empty is what to say when there are no rows. "No servers" is an answer;
	// a blank screen is a question about whether the command worked.
	Empty string
}

// Print renders `data` as JSON or YAML, or `table` as a table.
//
// Both are passed because they are genuinely different renderings of the same
// answer, and building the table when the caller asked for JSON would be work
// thrown away on the large listings this exists for.
func (p *Printer) Print(data any, table func() Table) error {
	switch p.Format {
	case FormatJSON:
		return p.json(data)
	case FormatYAML:
		return p.yaml(data)
	default:
		return p.table(table())
	}
}

func (p *Printer) json(v any) error {
	enc := json.NewEncoder(p.Out)
	enc.SetIndent("", "  ")
	// HTML escaping off: a CLI's JSON is read by jq and by people, and
	// `<` where a `<` was is neither's idea of the value.
	enc.SetEscapeHTML(false)
	return enc.Encode(v)
}

func (p *Printer) yaml(v any) error {
	// Marshalled through JSON so the `json:` tags on the API's own types are
	// what names the fields. yaml.v3 alone would use the Go field names and
	// produce a document whose keys match nothing in the API reference.
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	out, err := yaml.JSONToYAML(raw)
	if err != nil {
		return err
	}
	_, err = p.Out.Write(out)
	return err
}

func (p *Printer) table(t Table) error {
	if len(t.Rows) == 0 {
		msg := t.Empty
		if msg == "" {
			msg = "Nothing to show."
		}
		fmt.Fprintln(p.Out, msg)
		return nil
	}
	w := tabwriter.NewWriter(p.Out, 0, 0, 3, ' ', 0)
	if len(t.Headers) > 0 {
		fmt.Fprintln(w, p.dim(strings.Join(upper(t.Headers), "\t")))
	}
	for _, row := range t.Rows {
		fmt.Fprintln(w, strings.Join(row, "\t"))
	}
	return w.Flush()
}

// Detail renders one resource as aligned key/value lines, which is what a
// person wants from `get` and what a table of one row is not.
func (p *Printer) Detail(data any, fields func() [][2]string) error {
	switch p.Format {
	case FormatJSON:
		return p.json(data)
	case FormatYAML:
		return p.yaml(data)
	}
	w := tabwriter.NewWriter(p.Out, 0, 0, 2, ' ', 0)
	for _, kv := range fields() {
		fmt.Fprintf(w, "%s\t%s\n", p.dim(kv[0]+":"), kv[1])
	}
	return w.Flush()
}

// Says prints a human sentence: progress, confirmation, the result of an
// action. It goes to STDERR when the output is machine-readable, so a script
// piping stdout to jq is not handed prose in the middle of its JSON.
func (p *Printer) Says(format string, a ...any) {
	w := p.Out
	if p.Format != FormatTable {
		w = p.Err
	}
	fmt.Fprintf(w, format+"\n", a...)
}

// Warns is Says for something the reader should not miss. Always to stderr.
func (p *Printer) Warns(format string, a ...any) {
	fmt.Fprintln(p.Err, p.paint("33", "warning: ")+fmt.Sprintf(format, a...))
}

func (p *Printer) dim(s string) string  { return p.paint("2", s) }
func (p *Printer) Good(s string) string { return p.paint("32", s) }
func (p *Printer) Bad(s string) string  { return p.paint("31", s) }
func (p *Printer) Warn(s string) string { return p.paint("33", s) }

func (p *Printer) paint(code, s string) string {
	if !p.Color {
		return s
	}
	return "\x1b[" + code + "m" + s + "\x1b[0m"
}

// StateColor renders a lifecycle state in the colour its meaning deserves,
// using the SDK's own classification rather than a second opinion about which
// values are good. That is what keeps it right when the API adds a state.
func (p *Printer) StateColor(kind, state string) string {
	switch classify(kind, state) {
	case outcomeReady:
		return p.Good(state)
	case outcomeFailed:
		return p.Bad(state)
	case outcomeWorking:
		return p.Warn(state)
	default:
		return state
	}
}

func upper(v []string) []string {
	out := make([]string, len(v))
	for i, s := range v {
		out[i] = strings.ToUpper(s)
	}
	return out
}

// ago renders a timestamp the way a person reads it in a terminal.
func ago(t *time.Time) string {
	if t == nil {
		return "-"
	}
	d := time.Since(*t)
	switch {
	case d < 0:
		return t.Format("2006-01-02 15:04")
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}

// money renders a minor-unit amount. A bare integer of kurus in a terminal is a
// number somebody misreads by a factor of a hundred.
func money(minor int64, currency string) string {
	sign := ""
	if minor < 0 {
		sign, minor = "-", -minor
	}
	if currency == "" {
		currency = "TRY"
	}
	return fmt.Sprintf("%s%d.%02d %s", sign, minor/100, minor%100, currency)
}

// deref renders an optional string for a column.
func deref(s *string) string {
	if s == nil || *s == "" {
		return "-"
	}
	return *s
}
