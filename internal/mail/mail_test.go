package mail

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"io"
	"math/big"
	"mime/quotedprintable"
	"net"
	"net/mail"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

var linkRe = regexp.MustCompile(`https?://\S+`)

func TestTemplates(t *testing.T) {
	link := "https://bridge.example.org/auth/email/callback?token=abc"
	cases := []struct {
		m       Message
		kind    string
		subject string
		links   int
		says    []string
	}{
		{SignIn("anna@acme.ch", "Acme Team", link, 15*time.Minute), KindSignIn, "Sign in to Acme Team", 1, []string{"expires in 15 minutes", "works once"}},
		{Request("max@example.net", "Acme Team"), KindRequest, "Your request for access to Acme Team", 0, []string{"sent to the people who approve access"}},
		{Approve("boss@acme.ch", "Acme Team", "max@example.net", link, 7*24*time.Hour), KindApprove, "max@example.net asks for access to Acme Team", 1,
			[]string{"max@example.net asks for access to Acme Team", "expires in 7 days", "ignore this email"}},
		{Approved("max@example.net", "Acme Team", link, time.Hour), KindApproved, "You now have access to Acme Team", 1, []string{"expires in 1 hour"}},
	}
	for _, c := range cases {
		if c.m.Kind != c.kind || c.m.Subject != c.subject {
			t.Errorf("%s: kind %q subject %q", c.kind, c.m.Kind, c.m.Subject)
		}
		if got := linkRe.FindAllString(c.m.Body, -1); len(got) != c.links || (c.links == 1 && got[0] != link) {
			t.Errorf("%s: links %v, want %d", c.kind, got, c.links)
		}
		for _, s := range c.says {
			if !strings.Contains(c.m.Body, s) {
				t.Errorf("%s: body lacks %q:\n%s", c.kind, s, c.m.Body)
			}
		}
		if strings.Contains(c.m.Body, "{{") || strings.HasPrefix(c.m.Body, "\n") || strings.Contains(c.m.Body, "<") {
			t.Errorf("%s: body not plain text:\n%s", c.kind, c.m.Body)
		}
	}
	// Names cannot add headers, and an empty bridge name has a fallback.
	m := Approve("boss@acme.ch", "Evil\r\nBcc: x@y.z", "max@example.net\r\nBcc: x@y.z", link, time.Hour)
	if strings.ContainsAny(m.Subject, "\r\n") {
		t.Errorf("subject: %q", m.Subject)
	}
	if s := SignIn("a@b.ch", "", link, time.Minute).Subject; s != "Sign in to whatsapp-bridge" {
		t.Errorf("fallback name: %q", s)
	}
	for d, want := range map[time.Duration]string{15 * time.Minute: "15 minutes", time.Minute: "1 minute", time.Hour: "1 hour",
		2 * time.Hour: "2 hours", 24 * time.Hour: "1 day", 7 * 24 * time.Hour: "7 days", 90 * time.Second: "2 minutes"} {
		if got := Duration(d); got != want {
			t.Errorf("Duration(%v) = %q, want %q", d, got, want)
		}
	}
}

func TestConfigFromEnv(t *testing.T) {
	env := map[string]string{"BRIDGE_SMTP_HOST": " mail.example.ch ", "BRIDGE_SMTP_USER": "bridge@example.ch", "BRIDGE_SMTP_PASSWORD": " p w ",
		"BRIDGE_MAIL_FROM": "whatsapp-bridge <bridge@example.com>"}
	c, err := ConfigFromEnv(func(k string) string { return env[k] })
	if err != nil || c.Host != "mail.example.ch" || c.Port != 587 || c.Password != " p w " || !c.Configured() {
		t.Fatalf("config: %+v %v", c, err)
	}
	s, err := New(c)
	smtpS, ok := s.(*SMTP)
	if err != nil || !ok || smtpS.implicit || smtpS.from.Address != "bridge@example.com" {
		t.Fatalf("New: %T %v", s, err)
	}
	env["BRIDGE_SMTP_PORT"] = "465"
	c, _ = ConfigFromEnv(func(k string) string { return env[k] })
	if s, _ := New(c); !s.(*SMTP).implicit {
		t.Fatal("port 465 must use implicit TLS")
	}
	env["BRIDGE_SMTP_PORT"] = "smtp"
	if _, err := ConfigFromEnv(func(k string) string { return env[k] }); err == nil {
		t.Fatal("bad port accepted")
	}
	if s, err := New(Config{}); s != nil || err != nil {
		t.Fatalf("unconfigured: %v %v", s, err)
	}
	if _, err := New(Config{Host: "mail.example.ch", From: "not an address"}); err == nil {
		t.Fatal("bad From accepted")
	}
	// The dev file wins over SMTP ("instead of sending").
	if s, _ := New(Config{Host: "mail.example.ch", From: "a@b.ch", DevFile: "/tmp/x"}); s == nil {
		t.Fatal("dev file")
	} else if _, ok := s.(*DevFile); !ok {
		t.Fatalf("dev file sender: %T", s)
	}
}

// ---- a fake SMTP server ------------------------------------------------------------

type session struct {
	helo       []string
	authTLS    bool // AUTH arrived over TLS
	authPlain  string
	from, rcpt string
	data       string
	commands   []string
}

type fakeSMTP struct {
	t        *testing.T
	ln       net.Listener
	cfg      *tls.Config
	roots    *x509.CertPool
	starttls bool        // offer STARTTLS
	implicit bool        // the listener is TLS already
	silent   atomic.Bool // accept and never greet

	mu   sync.Mutex
	sess []*session
}

func newFakeSMTP(t *testing.T, starttls, implicit bool) *fakeSMTP {
	t.Helper()
	cert, roots := selfSigned(t)
	f := &fakeSMTP{t: t, starttls: starttls, implicit: implicit, roots: roots,
		cfg: &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	if implicit {
		ln = tls.NewListener(ln, f.cfg)
	}
	f.ln = ln
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go f.serve(c)
		}
	}()
	return f
}

func (f *fakeSMTP) port() int { return f.ln.Addr().(*net.TCPAddr).Port }

func (f *fakeSMTP) sender() *SMTP {
	s := newSMTP(Config{Host: "127.0.0.1", Port: f.port(), User: "bridge@example.test", Password: "s3cret"},
		&mail.Address{Name: "whatsapp-bridge", Address: "bridge@example.test"})
	s.roots, s.implicit = f.roots, f.implicit
	return s
}

func (f *fakeSMTP) sessions() []*session {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]*session(nil), f.sess...)
}

func (f *fakeSMTP) serve(c net.Conn) {
	defer c.Close()
	if f.silent.Load() {
		_, _ = io.Copy(io.Discard, c)
		return
	}
	s := &session{}
	f.mu.Lock()
	f.sess = append(f.sess, s)
	f.mu.Unlock()
	_, isTLS := c.(*tls.Conn)
	r, w := bufio.NewReader(c), c
	say := func(line string) { _, _ = io.WriteString(w, line+"\r\n") }
	say("220 fake ESMTP")
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")
		verb := strings.ToUpper(strings.SplitN(line, " ", 2)[0])
		f.mu.Lock()
		s.commands = append(s.commands, verb)
		f.mu.Unlock()
		switch verb {
		case "EHLO":
			f.mu.Lock()
			s.helo = append(s.helo, line)
			f.mu.Unlock()
			if f.starttls && !isTLS {
				say("250-fake")
				say("250 STARTTLS")
			} else {
				say("250-fake")
				say("250 AUTH PLAIN")
			}
		case "STARTTLS":
			say("220 go ahead")
			tc := tls.Server(c, f.cfg)
			if err := tc.Handshake(); err != nil {
				return
			}
			c, isTLS = tc, true
			r, w = bufio.NewReader(tc), tc
		case "AUTH":
			parts := strings.Fields(line)
			dec, _ := base64.StdEncoding.DecodeString(parts[len(parts)-1])
			f.mu.Lock()
			s.authTLS, s.authPlain = isTLS, string(dec)
			f.mu.Unlock()
			say("235 ok")
		case "MAIL":
			f.mu.Lock()
			s.from = line
			f.mu.Unlock()
			say("250 ok")
		case "RCPT":
			f.mu.Lock()
			s.rcpt = line
			f.mu.Unlock()
			say("250 ok")
		case "DATA":
			say("354 go")
			var b strings.Builder
			for {
				l, err := r.ReadString('\n')
				if err != nil {
					return
				}
				if l == ".\r\n" {
					break
				}
				b.WriteString(l)
			}
			f.mu.Lock()
			s.data = b.String()
			f.mu.Unlock()
			say("250 queued")
		case "QUIT":
			say("221 bye")
			return
		default:
			say("502 no")
		}
	}
}

func selfSigned(t *testing.T) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "127.0.0.1"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	leaf, _ := x509.ParseCertificate(der)
	pool := x509.NewCertPool()
	pool.AddCert(leaf)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}, pool
}

func checkDelivery(t *testing.T, f *fakeSMTP, msg Message) {
	t.Helper()
	ss := f.sessions()
	if len(ss) != 1 {
		t.Fatalf("sessions: %d", len(ss))
	}
	s := ss[0]
	if !s.authTLS || s.authPlain != "\x00bridge@example.test\x00s3cret" {
		t.Fatalf("AUTH PLAIN over TLS: tls=%v %q", s.authTLS, s.authPlain)
	}
	if s.from != "MAIL FROM:<bridge@example.test>" || !strings.HasPrefix(s.rcpt, "RCPT TO:<anna@acme.ch>") {
		t.Fatalf("envelope: %q %q", s.from, s.rcpt)
	}
	head, body, ok := strings.Cut(s.data, "\r\n\r\n")
	if !ok {
		t.Fatalf("no header/body split:\n%s", s.data)
	}
	for _, want := range []string{`From: "whatsapp-bridge" <bridge@example.test>`, "To: anna@acme.ch", "Subject: " + msg.Subject,
		"MIME-Version: 1.0", "Content-Type: text/plain; charset=utf-8", "Content-Transfer-Encoding: quoted-printable",
		"Auto-Submitted: auto-generated", "Message-ID: <", "@example.test>", "Date: "} {
		if !strings.Contains(head, want) {
			t.Fatalf("header lacks %q:\n%s", want, head)
		}
	}
	if strings.Contains(strings.ReplaceAll(s.data, "\r\n", ""), "\n") {
		t.Fatal("bare LF in the message")
	}
	dec, err := io.ReadAll(quotedprintable.NewReader(strings.NewReader(body)))
	if err != nil {
		t.Fatal(err)
	}
	if got := linkRe.FindString(string(dec)); got != linkRe.FindString(msg.Body) {
		t.Fatalf("link after decoding: %q", got)
	}
}

func TestSMTPStartTLS(t *testing.T) {
	f := newFakeSMTP(t, true, false)
	msg := SignIn("anna@acme.ch", "Acme Team", "https://bridge.example.org/auth/email/callback?token="+strings.Repeat("Ab-_", 11), 15*time.Minute)
	if err := f.sender().Send(context.Background(), msg); err != nil {
		t.Fatal(err)
	}
	checkDelivery(t, f, msg)
	if s := f.sessions()[0]; len(s.helo) != 2 || s.commands[1] != "STARTTLS" {
		t.Fatalf("EHLO, STARTTLS, EHLO expected: %v", s.commands)
	}
}

func TestSMTPImplicitTLS(t *testing.T) {
	f := newFakeSMTP(t, false, true)
	msg := Approved("anna@acme.ch", "Acme Team", "https://bridge.example.org/auth/email/callback?token=x", time.Hour)
	if err := f.sender().Send(context.Background(), msg); err != nil {
		t.Fatal(err)
	}
	checkDelivery(t, f, msg)
}

func TestSMTPRefusesPlainText(t *testing.T) {
	f := newFakeSMTP(t, false, false) // no STARTTLS, no TLS
	err := f.sender().Send(context.Background(), SignIn("anna@acme.ch", "Acme Team", "https://x/y", time.Minute))
	if err == nil || !strings.Contains(err.Error(), "STARTTLS") {
		t.Fatalf("plain-text server: %v", err)
	}
	for _, c := range f.sessions()[0].commands {
		if c == "AUTH" || c == "MAIL" || c == "DATA" {
			t.Fatalf("sent %s without TLS", c)
		}
	}
	// An untrusted certificate is refused too.
	g := newFakeSMTP(t, true, false)
	s := g.sender()
	s.roots = x509.NewCertPool()
	if err := s.Send(context.Background(), SignIn("anna@acme.ch", "Acme Team", "https://x/y", time.Minute)); err == nil {
		t.Fatal("untrusted certificate accepted")
	}
}

func TestSMTPTimeoutAndBadRecipient(t *testing.T) {
	f := newFakeSMTP(t, true, false)
	f.silent.Store(true)
	s := f.sender()
	s.timeout = 300 * time.Millisecond
	begin := time.Now()
	if err := s.Send(context.Background(), SignIn("anna@acme.ch", "Acme Team", "https://x/y", time.Minute)); err == nil {
		t.Fatal("silent server: no error")
	}
	if d := time.Since(begin); d > 3*time.Second {
		t.Fatalf("timeout took %v", d)
	}
	if Timeout != 10*time.Second || newSMTP(Config{Host: "h", Port: 587}, &mail.Address{Address: "a@b.ch"}).timeout != Timeout {
		t.Fatal("SMTP timeout must be 10 s")
	}
	for _, to := range []string{"anna@acme.ch\r\nBcc: x@y.z", "Anna <anna@acme.ch>", "a@b.ch, c@d.ch", ""} {
		if err := s.Send(context.Background(), Message{To: to, Subject: "x", Body: "y"}); err == nil || !strings.Contains(err.Error(), "recipient") {
			t.Fatalf("recipient %q: %v", to, err)
		}
	}
}

func TestDevFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mail.txt")
	d := NewDevFile(path, &mail.Address{Name: "whatsapp-bridge", Address: "bridge@localhost"})
	link := "http://127.0.0.1:8099/auth/email/callback?token=abc"
	for i := range 2 {
		if err := d.Send(context.Background(), SignIn("anna"+strconv.Itoa(i)+"@acme.ch", "Acme Team", link, 15*time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", fi.Mode().Perm())
	}
	text := string(b)
	if strings.Count(text, "----- mail ") != 2 || strings.Count(text, link) != 2 || !strings.Contains(text, "kind=signin") ||
		!strings.Contains(text, "To: anna1@acme.ch") || !strings.Contains(text, "Subject: Sign in to Acme Team") {
		t.Fatalf("dev file:\n%s", text)
	}
}
