package mail

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"mime"
	"mime/quotedprintable"
	"net"
	"net/mail"
	"net/smtp"
	"os"
	"strconv"
	"strings"
	"time"
)

// SMTP delivers through a mail server with TLS: implicit TLS on port 465,
// STARTTLS on any other port. A server without STARTTLS is refused rather
// than used in plain text; credentials are sent only over TLS (PLAIN).
type SMTP struct {
	host     string
	port     int
	user     string
	password string
	from     *mail.Address
	helo     string
	timeout  time.Duration

	// Tests: trust these roots, and force implicit TLS on another port.
	roots    *x509.CertPool
	implicit bool
	now      func() time.Time
}

func newSMTP(c Config, from *mail.Address) *SMTP {
	helo, err := os.Hostname()
	if err != nil || helo == "" || strings.ContainsAny(helo, " \r\n") {
		helo = "localhost"
	}
	return &SMTP{host: c.Host, port: c.Port, user: c.User, password: c.Password, from: from, helo: helo,
		timeout: Timeout, implicit: c.Port == 465, now: time.Now}
}

// Send delivers one message within the sender's timeout (or ctx, if sooner).
func (s *SMTP) Send(ctx context.Context, m Message) error {
	if err := validRecipient(m.To); err != nil {
		return err
	}
	data := compose(s.from, m, s.now())
	deadline := time.Now().Add(s.timeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	ctx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()

	addr := net.JoinHostPort(s.host, strconv.Itoa(s.port))
	tlsCfg := &tls.Config{ServerName: s.host, MinVersion: tls.VersionTLS12, RootCAs: s.roots}
	dialer := &net.Dialer{}
	var conn net.Conn
	var err error
	if s.implicit {
		conn, err = (&tls.Dialer{NetDialer: dialer, Config: tlsCfg}).DialContext(ctx, "tcp", addr)
	} else {
		conn, err = dialer.DialContext(ctx, "tcp", addr)
	}
	if err != nil {
		return fmt.Errorf("mail: connect to %s: %w", addr, err)
	}
	_ = conn.SetDeadline(deadline)
	stop := context.AfterFunc(ctx, func() { _ = conn.SetDeadline(time.Unix(1, 0)) })
	defer stop()

	c, err := smtp.NewClient(conn, s.host)
	if err != nil {
		conn.Close()
		return fmt.Errorf("mail: greeting from %s: %w", addr, err)
	}
	defer c.Close()
	if err := c.Hello(s.helo); err != nil {
		return fmt.Errorf("mail: EHLO: %w", err)
	}
	if !s.implicit {
		if ok, _ := c.Extension("STARTTLS"); !ok {
			return errors.New("mail: the server does not offer STARTTLS; refusing to send without TLS")
		}
		if err := c.StartTLS(tlsCfg); err != nil {
			return fmt.Errorf("mail: STARTTLS: %w", err)
		}
	}
	if s.user != "" {
		if ok, _ := c.Extension("AUTH"); !ok {
			return errors.New("mail: the server offers no authentication; check BRIDGE_SMTP_HOST and BRIDGE_SMTP_PORT")
		}
		if err := c.Auth(smtp.PlainAuth("", s.user, s.password, s.host)); err != nil {
			return fmt.Errorf("mail: authenticate as %s: %w", s.user, err)
		}
	}
	if err := c.Mail(s.from.Address); err != nil {
		return fmt.Errorf("mail: MAIL FROM: %w", err)
	}
	if err := c.Rcpt(m.To); err != nil {
		return fmt.Errorf("mail: RCPT TO: %w", err)
	}
	w, err := c.Data()
	if err != nil {
		return fmt.Errorf("mail: DATA: %w", err)
	}
	if _, err := w.Write(data); err != nil {
		return fmt.Errorf("mail: write message: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("mail: message refused: %w", err)
	}
	return c.Quit()
}

// compose renders the RFC 5322 message: plain text, UTF-8, quoted-printable,
// CRLF line ends.
func compose(from *mail.Address, m Message, now time.Time) []byte {
	var b bytes.Buffer
	hdr := func(k, v string) { b.WriteString(k + ": " + v + "\r\n") }
	hdr("From", from.String())
	hdr("To", m.To)
	hdr("Subject", mime.QEncoding.Encode("utf-8", headerSafe(m.Subject)))
	hdr("Date", now.Format(time.RFC1123Z))
	hdr("Message-ID", "<"+messageID()+"@"+domainOf(from.Address)+">")
	hdr("MIME-Version", "1.0")
	hdr("Content-Type", "text/plain; charset=utf-8")
	hdr("Content-Transfer-Encoding", "quoted-printable")
	hdr("Auto-Submitted", "auto-generated")
	b.WriteString("\r\n")
	qp := quotedprintable.NewWriter(&b)
	body := strings.ReplaceAll(strings.ReplaceAll(m.Body, "\r\n", "\n"), "\n", "\r\n")
	_, _ = qp.Write([]byte(body))
	_ = qp.Close()
	return b.Bytes()
}

func messageID() string {
	r := make([]byte, 16)
	_, _ = rand.Read(r)
	return hex.EncodeToString(r)
}

func domainOf(addr string) string {
	if i := strings.LastIndexByte(addr, '@'); i >= 0 && i < len(addr)-1 {
		return addr[i+1:]
	}
	return "localhost"
}
