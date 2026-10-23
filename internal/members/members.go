// Package members decides who may sign in to the bridge and lets people in
// with only a work email:
//
//	POST /auth/email/start          {email, next} -> 202 (form: 303 /signin?sent=1); never says whether the address is a member
//	GET  /auth/email/callback?token -> 303 /auth/email/callback (token moved to a short cookie) or /signin?error=...
//	GET  /auth/email/callback       -> "Continue" page; POST signs in and redirects to next
//	GET  /auth/email/approve?token  -> 303 /auth/email/approve, or a page saying the link is expired or used
//	GET  /auth/email/approve        -> "Approve <email>?" page; POST approves and emails the requester a sign-in link
//	GET/POST /api/members, POST /api/members/{id}/approve, DELETE /api/members/{id}  (session only)
//
// Admission: an approved member, an address whose domain is in
// BRIDGE_ALLOWED_DOMAINS, or an address in BRIDGE_APPROVERS gets a sign-in
// link (15 minutes, single use). Anyone else becomes a pending member; the
// requester is told the request went to the approver and every approver gets
// an approve link (7 days, single use), at most once per 24 hours per
// address. An approval emails the requester a fresh sign-in link (1 hour).
//
// Links are 32 random bytes stored only as SHA-256 hashes. Opening a link
// never changes anything by itself: mail scanners fetch links, so the
// sign-in and the approval happen on a same-origin POST from the page the
// link leads to, and the token travels from the link to that POST in a
// short-lived HttpOnly cookie, never in a page.
//
// Routes are mounted on the web layer's mux (web.Options.ExtraRoutes), so the
// Host allowlist, security headers, cross-origin protection and principal
// resolution apply. The package works without Google or Microsoft sign-in
// and without mail (requests then wait for the Members page or the admin
// command).
package members

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/vid-t/whatsapp-bridge/internal/auth"
	"github.com/vid-t/whatsapp-bridge/internal/mail"
)

const (
	signinTTL     = 15 * time.Minute   // a link the person asked for
	accessTTL     = time.Hour          // the link sent after an approval or an invitation
	approveTTL    = 7 * 24 * time.Hour // an approver's link
	renotifyEvery = 24 * time.Hour
	linkCookieTTL = 10 * time.Minute

	maxPending       = 500 // beyond this, new requests are not recorded
	approverMailRate = 30  // approve mails per hour, all requests together
	queueSize        = 256
	workers          = 2
	jobTimeout       = 30 * time.Second
	maxBody          = 16 << 10
	pruneEvery       = 10 * time.Minute
)

// Deps are the bridge services this package uses.
type Deps struct {
	Auth *auth.Store
	// DB is the bridge database; the link table is created by New.
	DB *sql.DB
	// Mail sends the sign-in, request and approval mails; nil when mail is
	// not configured (no links are sent, requests still get recorded).
	Mail mail.Sender
	Log  *slog.Logger
	// Audit receives events (type, detail). Details never contain tokens or
	// links.
	Audit func(typ, detail string)
}

// Config is the admission configuration, normally from ConfigFromEnv.
type Config struct {
	// PublicURL is the bridge origin used in emailed links. Empty: from the
	// request (local development); the admin command passes its own.
	PublicURL string
	// AllowedDomains admit any address of these domains on first sign-in.
	AllowedDomains []string
	// Approvers receive access requests; they are admitted themselves.
	Approvers []string

	now func() time.Time // tests
}

// ConfigFromEnv reads BRIDGE_ALLOWED_DOMAINS and BRIDGE_APPROVERS.
func ConfigFromEnv(getenv func(string) string, publicURL string) Config {
	c := Config{PublicURL: publicURL}
	for _, d := range strings.Split(getenv("BRIDGE_ALLOWED_DOMAINS"), ",") {
		if d = strings.ToLower(strings.TrimPrefix(strings.TrimSpace(d), "@")); d != "" {
			c.AllowedDomains = append(c.AllowedDomains, d)
		}
	}
	for _, a := range strings.Split(getenv("BRIDGE_APPROVERS"), ",") {
		if a = strings.TrimSpace(a); a != "" {
			c.Approvers = append(c.Approvers, a)
		}
	}
	return c
}

// Server serves email sign-in and the members API.
type Server struct {
	auth       *auth.Store
	db         *sql.DB
	mail       mail.Sender
	log        *slog.Logger
	auditFn    func(typ, detail string)
	publicURL  string
	domains    map[string]bool
	approvers  []string
	isApprover map[string]bool
	now        func() time.Time
	limiter    *auth.Limiter

	jobs      chan func(context.Context)
	pending   sync.WaitGroup
	lastPrune atomic.Int64
}

// New creates the link table and starts the mail workers. An approver
// address that is not an email address is logged and skipped.
func New(deps Deps, cfg Config) (*Server, error) {
	if deps.Auth == nil || deps.DB == nil {
		return nil, errors.New("members: Auth and DB are required")
	}
	log := deps.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	s := &Server{
		auth:       deps.Auth,
		db:         deps.DB,
		mail:       deps.Mail,
		log:        log.With("module", "members"),
		auditFn:    deps.Audit,
		publicURL:  strings.TrimRight(strings.TrimSpace(cfg.PublicURL), "/"),
		domains:    map[string]bool{},
		isApprover: map[string]bool{},
		now:        cfg.now,
		limiter:    auth.NewLimiter(),
		jobs:       make(chan func(context.Context), queueSize),
	}
	if s.now == nil {
		s.now = func() time.Time { return time.Now().UTC() }
	}
	for _, d := range cfg.AllowedDomains {
		if d = strings.ToLower(strings.TrimPrefix(strings.TrimSpace(d), "@")); d != "" {
			s.domains[d] = true
		}
	}
	for _, a := range cfg.Approvers {
		e, ok := auth.NormalizeEmail(a)
		if !ok {
			s.log.Error("BRIDGE_APPROVERS: not an email address; skipped", "value", a)
			continue
		}
		if !s.isApprover[e] {
			s.isApprover[e] = true
			s.approvers = append(s.approvers, e)
		}
	}
	if _, err := s.db.ExecContext(context.Background(), schema); err != nil {
		return nil, fmt.Errorf("members: schema: %w", err)
	}
	for range workers {
		go s.worker()
	}
	return s, nil
}

// EmailEnabled reports whether email sign-in is offered (mail is configured).
func (s *Server) EmailEnabled() bool { return s.mail != nil }

// Approvers returns the configured approver addresses.
func (s *Server) Approvers() []string { return append([]string(nil), s.approvers...) }

// RegisterRoutes mounts email sign-in and the members API on the web
// layer's inner mux.
func (s *Server) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /auth/email/start", s.start)
	mux.HandleFunc("GET /auth/email/callback", s.callbackPage)
	mux.HandleFunc("POST /auth/email/callback", s.callback)
	mux.HandleFunc("GET /auth/email/approve", s.approvePage)
	mux.HandleFunc("POST /auth/email/approve", s.approve)
	mux.HandleFunc("GET /api/members", s.sessionOnly(s.listMembers))
	mux.HandleFunc("POST /api/members", s.sessionOnly(s.inviteMember))
	mux.HandleFunc("POST /api/members/{id}/approve", s.sessionOnly(s.approveMember))
	mux.HandleFunc("DELETE /api/members/{id}", s.sessionOnly(s.removeMember))
}

// Wait blocks until queued admissions and mails are done, or ctx ends. The
// admin command calls it before exiting; serve calls it on shutdown.
func (s *Server) Wait(ctx context.Context) error {
	done := make(chan struct{})
	go func() { s.pending.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// enqueue runs job off the request path. A full queue drops the job: the
// requester sees the same "check your email" either way.
func (s *Server) enqueue(what string, job func(context.Context)) {
	s.pending.Add(1)
	select {
	case s.jobs <- job:
	default:
		s.pending.Done()
		s.log.Warn("mail queue full; dropped", "job", what)
	}
}

func (s *Server) worker() {
	for job := range s.jobs {
		s.runJob(job)
	}
}

func (s *Server) runJob(job func(context.Context)) {
	ctx, cancel := context.WithTimeout(context.Background(), jobTimeout)
	defer cancel()
	defer s.pending.Done()
	defer func() {
		if v := recover(); v != nil {
			s.log.Error("background job panicked", "panic", fmt.Sprint(v))
		}
	}()
	job(ctx)
}

// send queues one mail. The body holds a link: it is never logged.
func (s *Server) send(m mail.Message) {
	if s.mail == nil {
		return
	}
	s.enqueue("mail "+m.Kind, func(ctx context.Context) {
		ctx, cancel := context.WithTimeout(ctx, mail.Timeout)
		defer cancel()
		if err := s.mail.Send(ctx, m); err != nil {
			s.log.Warn("send mail", "kind", m.Kind, "to", m.To, "err", err)
			return
		}
		s.audit("mail.sent", "kind="+m.Kind+" to="+m.To)
	})
}

// admittedBy reports whether an address may sign in without approval, and
// the approved_by value to record: "domain" or "approvers".
func (s *Server) admittedBy(email string) (string, bool) {
	switch {
	case s.domains[auth.EmailDomain(email)]:
		return "domain", true
	case s.isApprover[email]:
		return "approvers", true
	}
	return "", false
}

// base is the public origin for emailed links.
func (s *Server) base(r *http.Request) string {
	if s.publicURL != "" {
		return s.publicURL
	}
	scheme := "http"
	if auth.SecureRequest(r) {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

func (s *Server) name() string {
	if f := s.auth.Name(); f != "" {
		return f
	}
	return "whatsapp-bridge"
}

func (s *Server) audit(typ, detail string) {
	if s.auditFn != nil {
		s.auditFn(typ, detail)
	}
}
