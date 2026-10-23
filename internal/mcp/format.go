package mcp

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/vid-t/whatsapp-bridge/internal/wa"
)

const (
	maxResultBytes = 1 << 20 // total text per tool result
	maxFieldRunes  = 2000    // per message text
	maxNameRunes   = 200     // chat and sender names
	trailerRoom    = 512     // reserved for the truncation note
)

// ---- result builder -----------------------------------------------------------

// page accumulates a plain-text result under the 1 MB cap. Lines that would
// overflow are counted, not written, and a note says so.
type page struct {
	b       strings.Builder
	omitted int
}

func (p *page) addf(format string, args ...any) { p.add(fmt.Sprintf(format, args...)) }

// add writes one line if it fits and reports whether it did.
func (p *page) add(line string) bool {
	if p.omitted > 0 || p.b.Len()+len(line)+1 > maxResultBytes-trailerRoom {
		p.omitted++
		return false
	}
	p.b.WriteString(line)
	p.b.WriteByte('\n')
	return true
}

func (p *page) String() string {
	if p.omitted > 0 {
		return p.b.String() + fmt.Sprintf("[result truncated at 1 MB: %d more lines omitted; ask for fewer messages or a narrower window]\n", p.omitted)
	}
	return p.b.String()
}

// ---- message lines --------------------------------------------------------------

func citation(chatJID, msgID string) string { return "wa://" + chatJID + "/" + msgID }

const msgColumns = "format: [citation] time (UTC) | sender | kind | text"
const hitColumns = "format: [citation] time (UTC) | chat | sender | kind | text"

// messageLine renders one message on one line. Newlines inside the text are
// escaped so a message can never start a line of its own (and so cannot forge
// a citation).
func messageLine(m wa.Message, chat string) string {
	var b strings.Builder
	b.WriteString("[")
	b.WriteString(citation(m.ChatJID, m.ID))
	b.WriteString("] ")
	b.WriteString(fmtTime(m.Timestamp))
	if chat != "" {
		b.WriteString(" | ")
		b.WriteString(cleanName(chat))
	}
	b.WriteString(" | ")
	b.WriteString(senderLabel(m))
	b.WriteString(" | ")
	b.WriteString(kindLabel(m.Kind))
	b.WriteString(" | ")
	b.WriteString(cleanText(m.Text, maxFieldRunes))
	return b.String()
}

func kindLabel(kind string) string {
	if kind = cleanName(kind); kind == "" {
		return "text"
	}
	return kind
}

func senderLabel(m wa.Message) string {
	if m.FromMe {
		return "me"
	}
	if n := cleanName(m.SenderName); n != "" {
		return n
	}
	if user, _, _ := strings.Cut(m.SenderJID, "@"); user != "" {
		user, _, _ = strings.Cut(user, ":") // drop the device suffix
		return cleanName(user)
	}
	return "unknown"
}

// cleanText flattens text onto one line and truncates it to max runes.
func cleanText(s string, max int) string {
	s = strings.TrimSpace(s)
	total := utf8.RuneCountInString(s)
	if total > max {
		cut := 0
		for i := range s {
			if cut == max {
				s = s[:i]
				break
			}
			cut++
		}
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		i += size
		switch {
		case r == '\r':
			if i < len(s) && s[i] == '\n' {
				i++
			}
			b.WriteString(`\n`)
		case r == '\n', r == ' ', r == ' ':
			b.WriteString(`\n`)
		case r == '\t':
			b.WriteByte(' ')
		case r < 0x20, r == 0x7f, r == utf8.RuneError && size == 1, isBidiControl(r):
			// dropped: control characters, invalid bytes, direction overrides
		default:
			b.WriteRune(r)
		}
	}
	if total > max {
		fmt.Fprintf(&b, " … [truncated: %d more characters not shown]", total-max)
	}
	return b.String()
}

// cleanName is cleanText for short fields that sit between separators.
func cleanName(s string) string {
	s = cleanText(strings.ReplaceAll(strings.ReplaceAll(s, "\n", " "), "\r", " "), maxNameRunes)
	return strings.ReplaceAll(s, "|", "/")
}

func isBidiControl(r rune) bool {
	return (r >= 0x202a && r <= 0x202e) || (r >= 0x2066 && r <= 0x2069) || r == 0x200e || r == 0x200f || r == 0x061c
}

func fmtTime(t time.Time) string {
	if t.IsZero() {
		return "unknown-time"
	}
	return t.UTC().Format(time.RFC3339)
}

func fmtTimePtr(t *time.Time) string {
	if t == nil {
		return "never"
	}
	return fmtTime(*t)
}

// ago renders a coarse age such as "3 min ago".
func ago(now, t time.Time) string {
	d := now.Sub(t)
	switch {
	case d < 0:
		return "in the future"
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%d min ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%d h ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%d days ago", int(d.Hours()/24))
	}
}

func kindOf(c wa.Chat) string {
	if c.IsGroup {
		return "group"
	}
	return "dm"
}

func chatLabel(c wa.Chat) string {
	if n := cleanName(c.Name); n != "" {
		return n
	}
	return c.JID
}

// ---- dates ---------------------------------------------------------------------------

var (
	dateTimeLayouts = []string{
		time.RFC3339Nano,
		"2006-01-02T15:04:05Z0700",
		"2006-01-02T15:04Z07:00",
		"2006-01-02 15:04:05Z07:00",
		"2006-01-02T15:04:05",
		"2006-01-02T15:04",
		"2006-01-02 15:04:05",
		"2006-01-02 15:04",
	}
	dateLayouts = []string{"2006-01-02", "2006/01/02", "02.01.2006", "2.1.2006", "20060102"}
)

// parseWhen parses a time leniently. Times without a zone are UTC. dateOnly
// reports whether the input named a whole day.
func parseWhen(s string) (t time.Time, dateOnly bool, err error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, false, errors.New("empty time")
	}
	for _, l := range dateTimeLayouts {
		if t, err := time.Parse(l, s); err == nil {
			return t.UTC(), false, nil
		}
	}
	for _, l := range dateLayouts {
		if t, err := time.Parse(l, s); err == nil {
			return t.UTC(), true, nil
		}
	}
	if sec, err := strconv.ParseInt(s, 10, 64); err == nil && sec > 1e9 && sec < 1e11 {
		return time.Unix(sec, 0).UTC(), false, nil
	}
	return time.Time{}, false, fmt.Errorf("cannot read %q as a time; use RFC3339 (2026-10-18T14:00:00Z) or a date (2026-10-18)", s)
}

// ---- lenient arguments --------------------------------------------------------------

// optInt accepts a JSON number, a numeric string or null.
type optInt struct {
	set bool
	v   int
}

func (o *optInt) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	if len(b) == 0 || string(b) == "null" {
		*o = optInt{}
		return nil
	}
	if b[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		if s = strings.TrimSpace(s); s == "" {
			*o = optInt{}
			return nil
		}
		b = []byte(s)
	}
	f, err := strconv.ParseFloat(string(b), 64)
	if err != nil || f != f {
		return fmt.Errorf("not a number: %s", b)
	}
	switch {
	case f > 1e6:
		f = 1e6
	case f < -1e6:
		f = -1e6
	}
	*o = optInt{set: true, v: int(f)}
	return nil
}

// or returns the value, or def when absent.
func (o optInt) or(def int) int {
	if !o.set {
		return def
	}
	return o.v
}

// clamp returns def when absent or below 1, and max when above it.
func (o optInt) clamp(def, max int) int {
	v := o.or(def)
	if v < 1 {
		return def
	}
	if v > max {
		return max
	}
	return v
}

// decodeArgs unmarshals tool arguments leniently: absent or null arguments
// give the zero value, unknown fields are ignored.
func decodeArgs(raw json.RawMessage, dst any) error {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	if err := json.Unmarshal(raw, dst); err != nil {
		var te *json.UnmarshalTypeError
		if errors.As(err, &te) && te.Field != "" {
			return fmt.Errorf("argument %q has the wrong type (got %s)", te.Field, te.Value)
		}
		return fmt.Errorf("invalid arguments: %v", err)
	}
	return nil
}
