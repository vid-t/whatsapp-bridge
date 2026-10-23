package oauth

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Client ID Metadata Documents (draft-ietf-oauth-client-id-metadata-document):
// the client_id is an https URL whose JSON document describes the client. The
// bridge fetches it once per hour at most, from public addresses only.

const cimdCacheMax = 256

var errPrivateAddr = errors.New("address is not public")

type cimdDoc struct {
	ClientID     string   `json:"client_id"`
	ClientName   string   `json:"client_name"`
	RedirectURIs []string `json:"redirect_uris"`
	ClientSecret string   `json:"client_secret"`
}

type cimdEntry struct {
	cl    client
	err   error
	until time.Time
}

type cimdFetcher struct {
	client *http.Client
	allow  func(netip.Addr) bool
	now    func() time.Time

	mu    sync.Mutex
	cache map[string]cimdEntry
}

// newCIMDClient builds the fetcher's HTTP client: no proxy, no redirects, a
// 5 second budget, and a dial-time check of the connected IP, so a hostname
// that resolves (or rebinds) to a private address is refused.
func newCIMDClient(allow func(netip.Addr) bool, roots *x509.CertPool) *http.Client {
	dialer := &net.Dialer{
		Timeout: cimdTimeout,
		Control: func(_, address string, _ syscall.RawConn) error {
			return checkDialAddr(address, allow)
		},
	}
	tr := &http.Transport{
		Proxy:                  nil,
		DialContext:            dialer.DialContext,
		TLSClientConfig:        &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12},
		TLSHandshakeTimeout:    cimdTimeout,
		ResponseHeaderTimeout:  cimdTimeout,
		MaxResponseHeaderBytes: 16 << 10,
		DisableKeepAlives:      true,
		ForceAttemptHTTP2:      true,
	}
	return &http.Client{
		Transport: tr,
		Timeout:   cimdTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return errors.New("redirects are not followed")
		},
	}
}

func checkDialAddr(address string, allow func(netip.Addr) bool) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return errPrivateAddr
	}
	a, err := netip.ParseAddr(host)
	if err != nil || !allow(a) {
		return errPrivateAddr
	}
	return nil
}

var blockedPrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"), // carrier-grade NAT, Tailscale
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("64:ff9b:1::/48"),
	netip.MustParsePrefix("100::/64"),
	netip.MustParsePrefix("2001:db8::/32"),
}

// publicAddr reports whether a is a globally routable unicast address.
func publicAddr(a netip.Addr) bool {
	a = a.Unmap()
	if !a.IsValid() || a.IsLoopback() || a.IsPrivate() || a.IsUnspecified() || a.IsLinkLocalUnicast() ||
		a.IsLinkLocalMulticast() || a.IsInterfaceLocalMulticast() || a.IsMulticast() {
		return false
	}
	if a.Is4() && a == netip.AddrFrom4([4]byte{255, 255, 255, 255}) {
		return false
	}
	for _, p := range blockedPrefixes {
		if p.Contains(a) {
			return false
		}
	}
	// NAT64 (64:ff9b::/96) embeds an IPv4 address; judge that one.
	if p := netip.MustParsePrefix("64:ff9b::/96"); p.Contains(a) {
		b := a.As16()
		return publicAddr(netip.AddrFrom4([4]byte{b[12], b[13], b[14], b[15]}))
	}
	return true
}

// checkClientIDURL applies the CIMD rules to the client_id itself before any
// network access: https, a path, no dot segments, no fragment or credentials,
// and no loopback or private host.
func checkClientIDURL(raw string, allow func(netip.Addr) bool) (*url.URL, error) {
	if len(raw) > maxParamLen {
		return nil, userError{"The app's client_id is too long."}
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.Opaque != "" {
		return nil, userError{"The app's client_id must be an https URL."}
	}
	if u.User != nil || u.Fragment != "" || strings.Contains(raw, "#") {
		return nil, userError{"The app's client_id URL must not contain credentials or a fragment."}
	}
	if u.Path == "" || u.Path == "/" {
		return nil, userError{"The app's client_id URL must have a path."}
	}
	for _, seg := range strings.Split(u.Path, "/") {
		if seg == "." || seg == ".." {
			return nil, userError{"The app's client_id URL must not contain . or .. segments."}
		}
	}
	host := strings.ToLower(u.Hostname())
	if host == "localhost" || strings.HasSuffix(host, ".localhost") || strings.HasSuffix(host, ".local") || strings.HasSuffix(host, ".internal") {
		return nil, userError{"The app's identity document is on a private address; only public https addresses are accepted."}
	}
	if a, err := netip.ParseAddr(host); err == nil && !allow(a) {
		return nil, userError{"The app's identity document is on a private address; only public https addresses are accepted."}
	}
	return u, nil
}

// get returns the client described by the document at raw, from the cache
// when fresh (successes for an hour, failures for a minute).
func (f *cimdFetcher) get(ctx context.Context, raw string) (client, error) {
	u, err := checkClientIDURL(raw, f.allow)
	if err != nil {
		return client{}, err
	}
	now := f.now()
	f.mu.Lock()
	if e, ok := f.cache[raw]; ok && now.Before(e.until) {
		f.mu.Unlock()
		return e.cl, e.err
	}
	f.mu.Unlock()

	cl, err := f.fetch(ctx, raw, u)
	ttl := cimdTTL
	if err != nil {
		ttl = cimdFailTTL
		if ctx.Err() != nil {
			return client{}, err // the browser went away; do not cache
		}
	}
	f.mu.Lock()
	if len(f.cache) >= cimdCacheMax {
		for k, e := range f.cache {
			if !now.Before(e.until) {
				delete(f.cache, k)
			}
		}
		if len(f.cache) >= cimdCacheMax {
			f.cache = map[string]cimdEntry{}
		}
	}
	f.cache[raw] = cimdEntry{cl: cl, err: err, until: now.Add(ttl)}
	f.mu.Unlock()
	return cl, err
}

func (f *cimdFetcher) fetch(ctx context.Context, raw string, u *url.URL) (client, error) {
	ctx, cancel := context.WithTimeout(ctx, cimdTimeout)
	defer cancel()
	fail := func(reason string) (client, error) {
		return client{}, userError{fmt.Sprintf("The app's identity document at %s could not be used: %s.", raw, reason)}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
	if err != nil {
		return fail("the address is not valid")
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "whatsapp-bridge-OAuth/1")
	resp, err := f.client.Do(req)
	if err != nil {
		switch {
		case errors.Is(err, errPrivateAddr):
			return fail("it is on a private address; only public https addresses are accepted")
		case errors.Is(err, context.DeadlineExceeded) || isTimeout(err):
			return fail("the server did not answer within 5 seconds")
		}
		return fail("it could not be fetched")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fail(fmt.Sprintf("the server answered HTTP %d", resp.StatusCode))
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, cimdMaxBytes+1))
	if err != nil {
		return fail("it could not be read")
	}
	if len(body) > cimdMaxBytes {
		return fail("it is larger than 64 KB")
	}
	var doc cimdDoc
	if err := json.Unmarshal(body, &doc); err != nil {
		return fail("it is not a valid JSON client metadata document")
	}
	if doc.ClientID != raw {
		return fail("its client_id does not match the address it was fetched from")
	}
	if doc.ClientSecret != "" {
		return fail("it contains a client_secret, which a public document must not")
	}
	var uris []string
	for _, ru := range doc.RedirectURIs {
		if validRedirectURI(ru) == nil {
			uris = append(uris, ru)
		}
	}
	if len(uris) == 0 {
		return fail("it lists no usable redirect_uris (https, or http on a loopback address)")
	}
	name, ok := cleanName(doc.ClientName)
	if !ok || name == "" {
		name = u.Hostname()
	}
	return client{ID: raw, Name: name, RedirectURIs: uris, Document: true, Host: strings.ToLower(u.Hostname())}, nil
}

func isTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}
