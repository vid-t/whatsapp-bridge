// Command whatsapp-bridge is a read-only WhatsApp linked device that
// shares the chats you tick with AI assistants over MCP.
//
//	whatsapp-bridge serve              start the HTTP server (default)
//	whatsapp-bridge pair               pair a new device from the terminal via QR
//	whatsapp-bridge pair-code -phone N pair a new device with an 8-character code
//	whatsapp-bridge sessions           list stored sessions
//	whatsapp-bridge keygen             print a fresh database key and access code
//	whatsapp-bridge members list|invite <email> [name]|approve <email>|remove <email>
//	                               manage who may sign in, beside the running service
//
// Every command accepts -env-file <path>: a systemd-style environment file
// read before the flags, so `whatsapp-bridge members -env-file /etc/whatsapp-bridge/whatsapp-bridge-env list`
// sees exactly what the service sees.
//
// Configuration comes from flags or environment variables:
//
//	BRIDGE_ADDR         listen address            (default 127.0.0.1:8080)
//	BRIDGE_DATA_DIR     data directory            (default ./data)
//	BRIDGE_DB_KEY       64 hex chars; enables Adiantum encryption at rest
//	BRIDGE_DEVICE_NAME  name shown in Linked devices (default whatsapp-bridge)
//	BRIDGE_NAME         name shown in the web UI, sign-in pages, emails and to
//	                    MCP clients (default whatsapp-bridge)
//	BRIDGE_LOG_LEVEL    debug|info|warn|error     (default info)
//	BRIDGE_ALLOWED_HOSTS comma-separated Host headers to accept (default: loopback names)
//	BRIDGE_INSECURE_BIND set to 1 to allow a non-loopback listen address while
//	                    the UI has no authentication (never on a public server)
//	BRIDGE_ACCESS_CODE  gates the UI; unset = open mode (loopback only)
//	BRIDGE_INSECURE_PLAIN set to 1 to allow serving off loopback without BRIDGE_DB_KEY
//	                    (and with BRIDGE_MAIL_DEV_FILE)
//	BRIDGE_PUBLIC_URL   external base URL (https://bridge.example.org) used in MCP metadata,
//	                    as the OAuth issuer (https, or http on loopback only), in the
//	                    Google/Microsoft redirect URIs and in emailed links
//	BRIDGE_GOOGLE_CLIENT_ID, BRIDGE_GOOGLE_CLIENT_SECRET        enable "Continue with Google"
//	BRIDGE_MICROSOFT_CLIENT_ID, BRIDGE_MICROSOFT_CLIENT_SECRET  enable "Continue with Microsoft"
//	BRIDGE_MICROSOFT_TENANT  common (default), organizations, consumers or a tenant id
//	BRIDGE_ALLOWED_DOMAINS   comma-separated email domains admitted without a member entry
//	BRIDGE_SMTP_HOST, BRIDGE_SMTP_PORT (587 STARTTLS | 465 TLS), BRIDGE_SMTP_USER,
//	BRIDGE_SMTP_PASSWORD, BRIDGE_MAIL_FROM   mail for email sign-in ("email" in /api/me)
//	BRIDGE_APPROVERS    comma-separated addresses that receive access requests
//	BRIDGE_MAIL_DEV_FILE development only: append mails to this file instead of sending
package main

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime/debug"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	qrcode "github.com/skip2/go-qrcode"

	"github.com/vid-t/whatsapp-bridge/internal/auth"
	"github.com/vid-t/whatsapp-bridge/internal/mail"
	"github.com/vid-t/whatsapp-bridge/internal/mcp"
	"github.com/vid-t/whatsapp-bridge/internal/members"
	"github.com/vid-t/whatsapp-bridge/internal/oauth"
	"github.com/vid-t/whatsapp-bridge/internal/sso"
	"github.com/vid-t/whatsapp-bridge/internal/store"
	"github.com/vid-t/whatsapp-bridge/internal/wa"
	"github.com/vid-t/whatsapp-bridge/internal/web"
)

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func main() {
	cmd := "serve"
	args := os.Args[1:]
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd, args = args[0], args[1:]
	}
	// The env file is read before the flags so its values become their defaults.
	if path := envFileArg(args); path != "" {
		if err := loadEnvFile(path); err != nil {
			fmt.Fprintln(os.Stderr, "whatsapp-bridge:", err)
			os.Exit(1)
		}
	}
	fs := flag.NewFlagSet("whatsapp-bridge "+cmd, flag.ExitOnError)
	fs.String("env-file", "", "systemd-style environment file read before the flags")
	addr := fs.String("addr", envOr("BRIDGE_ADDR", "127.0.0.1:8080"), "listen address")
	dataDir := fs.String("data", envOr("BRIDGE_DATA_DIR", "data"), "data directory")
	dbKey := fs.String("db-key", os.Getenv("BRIDGE_DB_KEY"), "64 hex chars; enables encryption at rest")
	deviceName := fs.String("device-name", envOr("BRIDGE_DEVICE_NAME", "whatsapp-bridge"), "name shown in Linked devices")
	logLevel := fs.String("log-level", envOr("BRIDGE_LOG_LEVEL", "info"), "debug|info|warn|error")
	phone := fs.String("phone", "", "phone number with country code (pair-code)")
	label := fs.String("label", "", "label for the new session (pair, pair-code)")
	allowedHosts := fs.String("allowed-hosts", os.Getenv("BRIDGE_ALLOWED_HOSTS"), "comma-separated Host headers to accept")
	insecureBind := fs.Bool("insecure-bind", os.Getenv("BRIDGE_INSECURE_BIND") == "1", "allow a non-loopback listen address without authentication")
	insecurePlain := fs.Bool("insecure-plain", os.Getenv("BRIDGE_INSECURE_PLAIN") == "1", "allow a non-loopback listen address without a database key")
	accessCode := fs.String("access-code", os.Getenv("BRIDGE_ACCESS_CODE"), "access code that gates the UI (unset = open mode)")
	name := fs.String("name", envOr("BRIDGE_NAME", "whatsapp-bridge"), "name shown in the web UI, emails and to MCP clients")
	publicURL := fs.String("public-url", os.Getenv("BRIDGE_PUBLIC_URL"), "external base URL used in MCP metadata and as the OAuth issuer")
	_ = fs.Parse(args)

	if cmd == "keygen" {
		fmt.Printf("BRIDGE_DB_KEY=%s\nBRIDGE_ACCESS_CODE=%s\n", randomHex(32), randomHex(24))
		return
	}

	var lvl slog.Level
	if err := lvl.UnmarshalText([]byte(*logLevel)); err != nil {
		lvl = slog.LevelInfo
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: lvl}))

	if cmd == "members" {
		// Opens the database without the WhatsApp manager, so it never
		// touches the linked devices of the service running beside it.
		os.Exit(runMembers(fs.Args(), membersConfig{dataDir: *dataDir, dbKey: *dbKey, accessCode: *accessCode,
			name: *name, publicURL: *publicURL, addr: *addr}, logger))
	}

	if err := os.MkdirAll(*dataDir, 0o700); err != nil {
		fatal(logger, "create data dir", err)
	}
	db, err := store.Open(filepath.Join(*dataDir, "bridge.db"), *dbKey)
	if err != nil {
		fatal(logger, "open database", err)
	}
	defer db.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	mgr, err := wa.NewManager(ctx, db, logger, wa.Options{DeviceName: *deviceName})
	if err != nil {
		fatal(logger, "init manager", err)
	}
	defer mgr.Close()

	switch cmd {
	case "serve":
		if !isLoopback(*addr) {
			if *accessCode == "" && !*insecureBind {
				fatal(logger, "refusing a non-loopback address without BRIDGE_ACCESS_CODE; set the code or -insecure-bind on a private network", errors.New(*addr))
			}
			if *dbKey == "" && !*insecurePlain {
				fatal(logger, "refusing a non-loopback address without BRIDGE_DB_KEY; run `whatsapp-bridge keygen` or set -insecure-plain", errors.New(*addr))
			}
		}
		if *dbKey == "" {
			logger.Warn("database is not encrypted at rest (BRIDGE_DB_KEY unset)")
		}
		if *accessCode == "" {
			logger.Warn("open mode: no access code; anyone who can reach this address can use the UI")
		}
		authStore, err := auth.New(ctx, db, auth.Options{AccessCode: *accessCode, Name: *name})
		if err != nil {
			fatal(logger, "init auth", err)
		}
		sender, err := mailSender(logger)
		if err != nil {
			// The bridge still serves its chats; only email sign-in is off.
			logger.Error("email sign-in disabled", "err", err)
		}
		if devFile := os.Getenv("BRIDGE_MAIL_DEV_FILE"); devFile != "" {
			if !isLoopback(*addr) && !*insecurePlain {
				fatal(logger, "refusing BRIDGE_MAIL_DEV_FILE on a non-loopback address: the file holds working sign-in links; set SMTP or -insecure-plain", errors.New(*addr))
			}
			if sender != nil {
				logger.Warn("development mail: sign-in and approval mails are appended to a file, not sent", "file", devFile)
			}
		}
		audit := func(typ, detail string) {
			actx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			mgr.Audit(actx, typ, detail)
		}
		memSrv, err := members.New(members.Deps{Auth: authStore, DB: db, Mail: sender, Log: logger, Audit: audit},
			members.ConfigFromEnv(os.Getenv, *publicURL))
		if err != nil {
			fatal(logger, "init members", err)
		}
		mgr.Start()
		runServe(ctx, *addr, serveConfig{
			allowedHosts: splitHosts(*allowedHosts),
			auth:         authStore,
			version:      buildVersion(),
			name:         *name,
			publicURL:    *publicURL,
			db:           db,
			members:      memSrv,
			audit:        audit,
		}, mgr, logger)
	case "pair":
		// Deliberately no mgr.Start(): connecting the other stored devices
		// here would collide with a running `serve` on the same database.
		runPairQR(ctx, mgr, *label, logger)
	case "pair-code":
		runPairCode(ctx, mgr, *phone, *label, logger)
	case "sessions":
		for _, s := range mgr.Sessions() {
			fmt.Printf("%s  %-12s  %s  %s  msgs=%d chats=%d  %s\n", s.ID, s.State, s.Phone, s.PushName, s.MessageCount, s.ChatCount, s.LastError)
		}
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q (serve|pair|pair-code|sessions|keygen|members)\n", cmd)
		os.Exit(2)
	}
}

func fatal(l *slog.Logger, msg string, err error) {
	l.Error(msg, "err", err)
	os.Exit(1)
}

func isLoopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func splitHosts(s string) []string {
	var out []string
	for _, h := range strings.Split(s, ",") {
		if h = strings.TrimSpace(h); h != "" {
			out = append(out, h)
		}
	}
	return out
}

type serveConfig struct {
	allowedHosts []string
	auth         *auth.Store
	version      string
	name         string
	publicURL    string
	db           *sql.DB
	members      *members.Server
	audit        func(typ, detail string)
}

// hostAllowed mirrors the web layer's allowlist for routes mounted outside it:
// loopback names always, configured names on any port, host:port entries exactly.
func hostAllowed(host string, extra []string) bool {
	host = strings.ToLower(host)
	name := host
	if h, _, err := net.SplitHostPort(host); err == nil {
		name = h
	}
	name = strings.Trim(name, "[]")
	switch name {
	case "localhost", "127.0.0.1", "::1":
		return true
	}
	for _, e := range extra {
		e = strings.ToLower(strings.TrimSpace(e))
		if e == "" {
			continue
		}
		if _, _, err := net.SplitHostPort(e); err == nil {
			if e == host {
				return true
			}
			continue
		}
		if strings.Trim(e, "[]") == name {
			return true
		}
	}
	return false
}

// guardHost rejects unexpected Host headers (DNS rebinding) and sets the
// headers the web layer sets, for handlers mounted beside it.
func guardHost(allowed []string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !hostAllowed(r.Host, allowed) {
			http.Error(w, `{"error":"unexpected Host header"}`, http.StatusForbidden)
			return
		}
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

// buildVersion returns the VCS revision baked in by the Go toolchain, or "dev".
func buildVersion() string {
	if info, ok := debug.ReadBuildInfo(); ok {
		rev, dirty := "", ""
		for _, kv := range info.Settings {
			switch kv.Key {
			case "vcs.revision":
				rev = kv.Value
			case "vcs.modified":
				if kv.Value == "true" {
					dirty = "-dirty"
				}
			}
		}
		if rev != "" {
			if len(rev) > 12 {
				rev = rev[:12]
			}
			return rev + dirty
		}
	}
	return "dev"
}

func runServe(ctx context.Context, addr string, cfg serveConfig, mgr *wa.Manager, logger *slog.Logger) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		// Ready when the database answers; linked devices may still be connecting.
		if _, err := mgr.RecentLogs(r.Context(), "", 1); err != nil {
			http.Error(w, "database not ready", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ready\n"))
	})
	audit := cfg.audit

	// Members and email sign-in are always mounted: the members API works
	// without Google or Microsoft, and "email" is offered when mail is set.
	extraRoutes := []func(*http.ServeMux){cfg.members.RegisterRoutes}
	var providers []string
	if cfg.members.EmailEnabled() {
		providers = append(providers, "email")
	}
	if len(cfg.members.Approvers()) == 0 {
		logger.Info("no BRIDGE_APPROVERS: access requests wait on the Members page or `whatsapp-bridge members approve`")
	}

	// Sign-in with Google and Microsoft (OIDC) next to the access code. A
	// provider is on when both its client id and secret are set.
	ssoSrv, err := sso.New(sso.Deps{Auth: cfg.auth, Log: logger, Audit: audit}, sso.ConfigFromEnv(os.Getenv, cfg.publicURL))
	if err != nil {
		logger.Error("Google/Microsoft sign-in disabled", "err", err)
	} else {
		providers = append(providers, ssoSrv.Providers()...)
		extraRoutes = append(extraRoutes, ssoSrv.RegisterRoutes)
	}
	if len(providers) > 0 {
		logger.Info("sign-in providers", "enabled", strings.Join(providers, ","))
	}

	// OAuth authorization server for click-to-connect MCP clients, behind the
	// same Host allowlist. Bearer keys keep working without it.
	var verifier auth.Verifier = cfg.auth
	oauthMux := http.NewServeMux()
	oauthSrv, err := oauth.Register(oauthMux, oauth.Deps{DB: cfg.db, Auth: cfg.auth, Log: logger, Audit: audit},
		oauth.Options{PublicURL: cfg.publicURL, SignInProviders: providers})
	if err != nil {
		logger.Error("OAuth disabled; assistants can still connect with bearer keys", "err", err)
	} else {
		verifier = oauthSrv.Verifier(cfg.auth)
		extraRoutes = append(extraRoutes, oauthSrv.RegisterAPI)
		for _, p := range oauth.Paths() {
			mux.Handle(p, guardHost(cfg.allowedHosts, oauthMux))
		}
	}

	// MCP for AI assistants, behind the same Host allowlist.
	mcpMux := http.NewServeMux()
	mcp.Register(mcpMux, mgr, verifier, logger, mcp.Options{
		Name:      cfg.name,
		Version:   cfg.version,
		PublicURL: cfg.publicURL,
		OAuth:     oauthSrv != nil,
		Audit: func(e mcp.AuditEvent) {
			audit("mcp.tool", e.Detail())
		},
	})
	for _, p := range []string{"/mcp", "/mcp/", "/.well-known/oauth-protected-resource", "/.well-known/oauth-protected-resource/"} {
		mux.Handle(p, guardHost(cfg.allowedHosts, mcpMux))
	}

	web.RegisterWithOptions(mux, mgr, logger, web.Options{
		AllowedHosts:    cfg.allowedHosts,
		Auth:            cfg.auth,
		Version:         cfg.version,
		Audit:           audit,
		SignInProviders: providers,
		ExtraRoutes: func(m *http.ServeMux) {
			for _, register := range extraRoutes {
				register(m)
			}
		},
	})
	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	listen(ctx, srv, logger)
	// Let queued sign-in and approval mails go out.
	waitCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := cfg.members.Wait(waitCtx); err != nil {
		logger.Warn("mails still queued at shutdown were dropped")
	}
}

// listen serves until ctx ends (SIGINT, SIGTERM) or the server fails, then
// shuts down gracefully. It returns normally so deferred cleanup in main runs.
func listen(ctx context.Context, srv *http.Server, logger *slog.Logger) {
	errCh := make(chan error, 1)
	go func() {
		logger.Info("listening", "addr", "http://"+srv.Addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
		close(errCh)
	}()
	select {
	case err := <-errCh:
		if err != nil {
			logger.Error("http server", "err", err)
			return
		}
	case <-ctx.Done():
	}
	logger.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)
}

// mailSender builds the sender from BRIDGE_SMTP_* and BRIDGE_MAIL_*; nil
// when mail is not configured.
func mailSender(logger *slog.Logger) (mail.Sender, error) {
	cfg, err := mail.ConfigFromEnv(os.Getenv)
	if err != nil {
		return nil, err
	}
	sender, err := mail.New(cfg)
	if err != nil {
		return nil, err
	}
	switch {
	case cfg.DevFile != "":
	case sender != nil:
		logger.Info("mail", "smtp", net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port)))
	default:
		logger.Info("mail not configured (BRIDGE_SMTP_HOST unset): email sign-in is off")
	}
	return sender, nil
}

type membersConfig struct {
	dataDir, dbKey, accessCode, name, publicURL, addr string
}

const membersUsage = `usage: whatsapp-bridge members [-env-file F] list
       whatsapp-bridge members [-env-file F] invite <email> [name]
       whatsapp-bridge members [-env-file F] approve <email>
       whatsapp-bridge members [-env-file F] remove <email>`

// runMembers manages members of the instance whose database is in the data
// directory, beside the running service (SQLite WAL). Invitations and
// approvals email a sign-in link when mail is configured; changes are
// audited with by=admin.
func runMembers(args []string, c membersConfig, logger *slog.Logger) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, membersUsage)
		return 2
	}
	path := filepath.Join(c.dataDir, "bridge.db")
	if _, err := os.Stat(path); err != nil {
		fmt.Fprintf(os.Stderr, "no database at %s; check BRIDGE_DATA_DIR or -env-file\n", path)
		return 1
	}
	db, err := store.Open(path, c.dbKey)
	if err != nil {
		fmt.Fprintln(os.Stderr, "open database (is BRIDGE_DB_KEY right?):", err)
		return 1
	}
	defer db.Close()
	ctx := context.Background()
	authStore, err := auth.New(ctx, db, auth.Options{AccessCode: c.accessCode, Name: c.name})
	if err != nil {
		fmt.Fprintln(os.Stderr, "init auth:", err)
		return 1
	}
	sender, err := mailSender(slog.New(slog.DiscardHandler))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	audit := func(typ, detail string) {
		if err := wa.AppendAudit(ctx, db, typ, detail); err != nil {
			logger.Warn("audit", "err", err)
		}
	}
	srv, err := members.New(members.Deps{Auth: authStore, DB: db, Mail: sender, Log: logger, Audit: audit},
		members.ConfigFromEnv(os.Getenv, c.publicURL))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	base := strings.TrimRight(c.publicURL, "/")
	if base == "" {
		base = "http://" + c.addr
	}
	const by = "admin"
	byEmail := func(email string) (auth.Member, bool) {
		m, err := authStore.MemberByEmail(ctx, email)
		switch {
		case errors.Is(err, auth.ErrBadEmail):
			fmt.Fprintf(os.Stderr, "%q is not an email address\n", email)
		case errors.Is(err, auth.ErrNoMember):
			fmt.Fprintf(os.Stderr, "%s is not a member or a pending request\n", email)
		case err != nil:
			fmt.Fprintln(os.Stderr, err)
		default:
			return m, true
		}
		return auth.Member{}, false
	}
	mailNote := func() string {
		if sender == nil {
			return " Mail is not configured: tell them to sign in at " + base + "."
		}
		return " A sign-in link (valid 1 hour) is on its way to them."
	}

	status := 0
	switch {
	case args[0] == "list" && len(args) == 1:
		ms, err := authStore.ListMembers(ctx)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "STATUS\tEMAIL\tNAME\tSIGN-IN\tLAST SIGN-IN\tAPPROVED BY")
		for _, m := range ms {
			last := "-"
			if m.LastLoginAt != nil {
				last = m.LastLoginAt.UTC().Format("2006-01-02 15:04")
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", m.Status, m.Email, dash(m.Name), dash(m.Provider), last, dash(m.ApprovedBy))
		}
		_ = tw.Flush()
	case args[0] == "invite" && len(args) >= 2:
		m, err := srv.Invite(ctx, args[1], strings.Join(args[2:], " "), by, base)
		switch {
		case errors.Is(err, auth.ErrBadEmail):
			fmt.Fprintf(os.Stderr, "%q is not an email address\n", args[1])
			status = 1
		case errors.Is(err, auth.ErrMemberExists):
			fmt.Fprintf(os.Stderr, "%s is already a member\n", args[1])
			status = 1
		case err != nil:
			fmt.Fprintln(os.Stderr, err)
			status = 1
		default:
			fmt.Printf("Invited %s.%s\n", m.Email, mailNote())
		}
	case args[0] == "approve" && len(args) == 2:
		m, ok := byEmail(args[1])
		if !ok {
			return 1
		}
		m, changed, err := srv.Approve(ctx, m.ID, by, base)
		switch {
		case err != nil:
			fmt.Fprintln(os.Stderr, err)
			status = 1
		case !changed:
			fmt.Printf("%s was already approved.\n", m.Email)
		default:
			fmt.Printf("Approved %s.%s\n", m.Email, mailNote())
		}
	case args[0] == "remove" && len(args) == 2:
		m, ok := byEmail(args[1])
		if !ok {
			return 1
		}
		if _, err := srv.Remove(ctx, m.ID, by); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		fmt.Printf("Removed %s (%s); their sessions are signed out.\n", m.Email, m.Status)
	default:
		fmt.Fprintln(os.Stderr, membersUsage)
		return 2
	}
	waitCtx, cancel := context.WithTimeout(ctx, 2*mail.Timeout)
	defer cancel()
	if err := srv.Wait(waitCtx); err != nil {
		fmt.Fprintln(os.Stderr, "the mail did not go out in time")
		status = 1
	}
	return status
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

var envKeyRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// envFileArg finds -env-file (or --env-file) among the flags.
func envFileArg(args []string) string {
	for i, a := range args {
		if a == "--" {
			break
		}
		for _, name := range []string{"-env-file", "--env-file"} {
			if a == name && i+1 < len(args) {
				return args[i+1]
			}
			if v, ok := strings.CutPrefix(a, name+"="); ok {
				return v
			}
		}
	}
	return ""
}

// loadEnvFile sets the variables of a systemd EnvironmentFile in this
// process, parsed as systemd does: KEY=VALUE lines, # or ; comments, values
// in double quotes (\" \\ \$ \` escapes), single quotes (literal) or bare
// (a backslash escapes the next character). Values in the file win over the
// inherited environment, as for the service.
// unescape drops a backslash before any character in only, or before any
// character at all when only is "".
func unescape(s, only string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) && (only == "" || strings.IndexByte(only, s[i+1]) >= 0) {
			i++
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func loadEnvFile(path string) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	for i, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(strings.TrimSuffix(line, "\r"))
		if line == "" || line[0] == '#' || line[0] == ';' {
			continue
		}
		k, v, ok := strings.Cut(strings.TrimPrefix(line, "export "), "=")
		k = strings.TrimSpace(k)
		if !ok || !envKeyRe.MatchString(k) {
			return fmt.Errorf("%s:%d: expected KEY=VALUE", path, i+1)
		}
		v = strings.TrimSpace(v)
		switch {
		case len(v) >= 2 && v[0] == '"' && v[len(v)-1] == '"':
			v = unescape(v[1:len(v)-1], "\"\\$`")
		case len(v) >= 2 && v[0] == '\'' && v[len(v)-1] == '\'':
			v = v[1 : len(v)-1]
		default:
			v = unescape(v, "")
		}
		if err := os.Setenv(k, v); err != nil {
			return err
		}
	}
	return nil
}

// runPairQR drives the QR flow from the terminal and then keeps running so
// history sync and live messages can be observed. Ctrl-C exits.
func runPairQR(ctx context.Context, mgr *wa.Manager, label string, logger *slog.Logger) {
	events, unsub := mgr.Subscribe()
	defer unsub()
	id, pairCh, err := mgr.StartPairing(label)
	if err != nil {
		fatal(logger, "start pairing", err)
	}
	fmt.Println("session:", id)
	for pe := range pairCh {
		switch pe.Type {
		case wa.PairQR:
			q, err := qrcode.New(pe.Code, qrcode.Medium)
			if err == nil {
				fmt.Println(q.ToSmallString(false))
			}
			fmt.Printf("Scan with WhatsApp > Linked devices > Link a device (expires in %ds)\n\n", pe.TimeoutSeconds)
		case wa.PairPasskeyRequest:
			fmt.Println("WhatsApp requested a passkey (WebAuthn) assertion. The terminal cannot answer it; try pair-code.")
		case wa.PairPasskeyCode:
			fmt.Println("Confirm this code on the phone:", pe.Code)
		case wa.PairScannedNoMultiDev:
			fmt.Println("Phone scanned without multi-device enabled; enable it and scan again.")
		case wa.PairSuccess:
			fmt.Println("Paired. Waiting for connection and history sync...")
		case wa.PairTimeout, wa.PairError, wa.PairCancelled:
			fmt.Println("Pairing ended:", pe.Type, pe.Error)
			return
		}
	}
	watch(ctx, events, mgr, id)
}

func runPairCode(ctx context.Context, mgr *wa.Manager, phone, label string, logger *slog.Logger) {
	if phone == "" {
		fatal(logger, "pair-code", errors.New("-phone is required"))
	}
	events, unsub := mgr.Subscribe()
	defer unsub()
	id, code, err := mgr.PairWithCode(ctx, phone, label)
	if err != nil {
		fatal(logger, "pair with code", err)
	}
	fmt.Println("session:", id)
	fmt.Println("On the phone: WhatsApp > Linked devices > Link a device > Link with phone number instead")
	fmt.Println("Enter this code:", code)
	watch(ctx, events, mgr, id)
}

// watch prints lifecycle events for a session until ctx is cancelled.
func watch(ctx context.Context, events <-chan wa.Event, mgr *wa.Manager, id string) {
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-events:
			if !ok {
				return
			}
			if ev.SessionID != id {
				continue
			}
			switch ev.Type {
			case wa.EventState:
				fmt.Printf("[%s] state=%s %s\n", ev.Time.Format(time.TimeOnly), ev.State, ev.Text)
				if ev.State == wa.StateConnected {
					if s, err := mgr.Session(id); err == nil {
						fmt.Printf("        linked as %s (%s) on %s\n", s.Phone, s.PushName, s.Platform)
					}
				}
			case wa.EventPair:
				if ev.Pair != nil {
					fmt.Printf("[%s] pair=%s %s %s\n", ev.Time.Format(time.TimeOnly), ev.Pair.Type, ev.Pair.Code, ev.Pair.Error)
				}
			case wa.EventMessage:
				if ev.Message != nil {
					fmt.Printf("[%s] message chat=%s kind=%s from=%s\n", ev.Time.Format(time.TimeOnly), ev.Message.ChatJID, ev.Message.Kind, ev.Message.SenderName)
				}
			default:
				fmt.Printf("[%s] %s %s\n", ev.Time.Format(time.TimeOnly), ev.Type, ev.Text)
			}
		}
	}
}
