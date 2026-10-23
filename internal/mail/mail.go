// Package mail sends the bridge's few plain-text emails: email sign-in
// links, access requests and approvals (see internal/members).
//
// A Sender delivers one Message. Two implementations exist: SMTP (STARTTLS
// on port 587, implicit TLS on 465, PLAIN authentication, never plain text)
// and DevFile, which appends mails to a local file for development. Senders
// are called off the request path; the caller bounds each send with a
// context, and the SMTP sender adds its own 10 s limit.
//
// Message bodies carry sign-in and approval links. Nothing in this package
// logs a body, and callers must not either.
package mail

import (
	"context"
	"errors"
	"fmt"
	"net/mail"
	"strconv"
	"strings"
	"time"
)

// Timeout bounds one SMTP conversation: connect, TLS, authentication and
// delivery.
const Timeout = 10 * time.Second

// Kinds of mail, as named in audit lines (mail.sent kind=...).
const (
	KindSignIn   = "signin"   // a sign-in link the person asked for
	KindRequest  = "request"  // tells a requester their request went to the approver
	KindApprove  = "approve"  // asks an approver to approve a request
	KindApproved = "approved" // tells a person they now have access, with a sign-in link
)

// Message is one plain-text mail to one recipient.
type Message struct {
	Kind    string
	To      string
	Subject string
	Body    string
}

// Sender delivers mail.
type Sender interface {
	Send(ctx context.Context, m Message) error
}

// Config is the mail configuration, normally from the environment
// (ConfigFromEnv).
type Config struct {
	Host     string // SMTP server; empty disables SMTP
	Port     int    // 587 (STARTTLS, default) or 465 (implicit TLS)
	User     string // PLAIN authentication when set
	Password string
	From     string // "whatsapp-bridge <bridge@example.com>" or a bare address
	// DevFile, when set, appends every mail to this file instead of sending
	// it. Development only; cmd/whatsapp-bridge refuses it off loopback.
	DevFile string
}

// ConfigFromEnv reads BRIDGE_SMTP_HOST, BRIDGE_SMTP_PORT, BRIDGE_SMTP_USER,
// BRIDGE_SMTP_PASSWORD, BRIDGE_MAIL_FROM and BRIDGE_MAIL_DEV_FILE.
func ConfigFromEnv(getenv func(string) string) (Config, error) {
	c := Config{
		Host:     strings.TrimSpace(getenv("BRIDGE_SMTP_HOST")),
		Port:     587,
		User:     strings.TrimSpace(getenv("BRIDGE_SMTP_USER")),
		Password: getenv("BRIDGE_SMTP_PASSWORD"),
		From:     strings.TrimSpace(getenv("BRIDGE_MAIL_FROM")),
		DevFile:  strings.TrimSpace(getenv("BRIDGE_MAIL_DEV_FILE")),
	}
	if p := strings.TrimSpace(getenv("BRIDGE_SMTP_PORT")); p != "" {
		n, err := strconv.Atoi(p)
		if err != nil || n < 1 || n > 65535 {
			return c, fmt.Errorf("mail: BRIDGE_SMTP_PORT must be a port number such as 587 or 465, got %q", p)
		}
		c.Port = n
	}
	return c, nil
}

// Configured reports whether mail can be sent (SMTP or the dev file).
func (c Config) Configured() bool { return c.Host != "" || c.DevFile != "" }

// New returns the sender for the configuration: the dev file when set,
// else SMTP, else nil (mail is not configured).
func New(c Config) (Sender, error) {
	from, err := c.from()
	switch {
	case c.DevFile != "":
		if err != nil {
			from = &mail.Address{Name: "whatsapp-bridge", Address: "bridge@localhost"}
		}
		return NewDevFile(c.DevFile, from), nil
	case c.Host == "":
		return nil, nil
	case err != nil:
		return nil, err
	}
	return newSMTP(c, from), nil
}

func (c Config) from() (*mail.Address, error) {
	raw := c.From
	if raw == "" {
		raw = c.User
	}
	a, err := mail.ParseAddress(raw)
	if err != nil || strings.ContainsAny(a.Address, "\r\n<>") {
		return nil, errors.New(`mail: BRIDGE_MAIL_FROM must be an address such as "whatsapp-bridge <bridge@example.com>"`)
	}
	return a, nil
}

// headerSafe removes line breaks so a value cannot add mail headers.
func headerSafe(s string) string {
	return strings.Join(strings.Fields(strings.NewReplacer("\r", " ", "\n", " ").Replace(s)), " ")
}

// validRecipient accepts one bare address.
func validRecipient(to string) error {
	a, err := mail.ParseAddress(to)
	if err != nil || a.Address != to || strings.ContainsAny(to, "\r\n") {
		return fmt.Errorf("mail: not a single bare recipient address")
	}
	return nil
}
