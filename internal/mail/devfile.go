package mail

import (
	"context"
	"fmt"
	"net/mail"
	"os"
	"strings"
	"sync"
	"time"
)

// DevFile appends each mail, readable as plain text, to a file instead of
// sending it, so sign-in can be tried locally without a mail server. The file
// holds working sign-in links: it is created 0600 and is for development on
// loopback only.
type DevFile struct {
	path string
	from *mail.Address
	mu   sync.Mutex
}

// NewDevFile returns a sender that appends to path.
func NewDevFile(path string, from *mail.Address) *DevFile {
	return &DevFile{path: path, from: from}
}

// Send appends the message.
func (d *DevFile) Send(_ context.Context, m Message) error {
	if err := validRecipient(m.To); err != nil {
		return err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "----- mail %s kind=%s\n", time.Now().UTC().Format(time.RFC3339), headerSafe(m.Kind))
	fmt.Fprintf(&b, "From: %s\nTo: %s\nSubject: %s\n\n", d.from.String(), m.To, headerSafe(m.Subject))
	b.WriteString(strings.TrimRight(m.Body, "\n"))
	b.WriteString("\n\n")
	d.mu.Lock()
	defer d.mu.Unlock()
	f, err := os.OpenFile(d.path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return fmt.Errorf("mail: dev file: %w", err)
	}
	if _, err := f.WriteString(b.String()); err != nil {
		f.Close()
		return fmt.Errorf("mail: dev file: %w", err)
	}
	return f.Close()
}
