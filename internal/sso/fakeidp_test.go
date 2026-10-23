package sso

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeIdP is a minimal OpenID Connect provider: discovery documents for a
// Google-like issuer (at the root) and a Microsoft-like multi-tenant issuer
// (under /common/v2.0 and /organizations/v2.0), a JWKS with a test RSA key,
// and a token endpoint that checks PKCE and issues RS256-signed ID tokens.
type fakeIdP struct {
	t   *testing.T
	ts  *httptest.Server
	key *rsa.PrivateKey

	mu    sync.Mutex
	codes map[string]grant
	hits  int // token requests
}

type grant struct {
	claims    map[string]any
	challenge string
	redirect  string
	clientID  string
}

func newFakeIdP(t *testing.T) *fakeIdP {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeIdP{t: t, key: key, codes: map[string]grant{}}
	mux := http.NewServeMux()
	f.ts = httptest.NewServer(mux)
	t.Cleanup(f.ts.Close)
	discovery := func(issuer string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"issuer":                                issuer,
				"authorization_endpoint":                f.ts.URL + "/authorize",
				"token_endpoint":                        f.ts.URL + "/token",
				"jwks_uri":                              f.ts.URL + "/keys",
				"response_types_supported":              []string{"code"},
				"subject_types_supported":               []string{"public"},
				"id_token_signing_alg_values_supported": []string{"RS256"},
			})
		}
	}
	mux.HandleFunc("GET /.well-known/openid-configuration", discovery(f.ts.URL))
	for _, tenant := range []string{"common", "organizations", "consumers"} {
		mux.HandleFunc("GET /"+tenant+"/v2.0/.well-known/openid-configuration", discovery(f.ts.URL+"/{tenantid}/v2.0"))
	}
	mux.HandleFunc("GET /keys", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{
			"kty": "RSA", "kid": "k1", "use": "sig", "alg": "RS256",
			"n": b64(key.N.Bytes()), "e": b64(big.NewInt(int64(key.E)).Bytes()),
		}}})
	})
	mux.HandleFunc("POST /token", f.token)
	return f
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func (f *fakeIdP) sign(claims map[string]any) string {
	hdr, _ := json.Marshal(map[string]string{"alg": "RS256", "kid": "k1", "typ": "JWT"})
	body, _ := json.Marshal(claims)
	in := b64(hdr) + "." + b64(body)
	sum := sha256.Sum256([]byte(in))
	sig, err := rsa.SignPKCS1v15(rand.Reader, f.key, crypto.SHA256, sum[:])
	if err != nil {
		f.t.Fatal(err)
	}
	return in + "." + b64(sig)
}

// authorize plays the provider's login page: it reads the authorization
// request the bridge redirected to, and returns a code for these claims.
// Standard claims (iss, aud, nonce, exp, iat) are filled in unless given.
func (f *fakeIdP) authorize(location string, claims map[string]any) (code string, q url.Values) {
	f.t.Helper()
	u, err := url.Parse(location)
	if err != nil || !strings.HasPrefix(location, f.ts.URL+"/authorize") {
		f.t.Fatalf("not an authorization request to the fake provider: %q", location)
	}
	q = u.Query()
	c := map[string]any{
		"iss": f.ts.URL, "aud": q.Get("client_id"), "nonce": q.Get("nonce"),
		"iat": time.Now().Unix(), "exp": time.Now().Add(time.Hour).Unix(),
	}
	for k, v := range claims {
		if v == nil {
			delete(c, k)
		} else {
			c[k] = v
		}
	}
	code = randomString(16)
	f.mu.Lock()
	f.codes[code] = grant{claims: c, challenge: q.Get("code_challenge"), redirect: q.Get("redirect_uri"), clientID: q.Get("client_id")}
	f.mu.Unlock()
	return code, q
}

func (f *fakeIdP) token(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	f.mu.Lock()
	f.hits++
	g, ok := f.codes[r.PostForm.Get("code")]
	delete(f.codes, r.PostForm.Get("code"))
	f.mu.Unlock()
	fail := func(code string) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": code})
	}
	clientID, secret, basic := r.BasicAuth()
	if !basic {
		clientID, secret = r.PostForm.Get("client_id"), r.PostForm.Get("client_secret")
	}
	sum := sha256.Sum256([]byte(r.PostForm.Get("code_verifier")))
	switch {
	case !ok:
		fail("invalid_grant")
	case clientID != g.clientID || secret == "":
		fail("invalid_client")
	case r.PostForm.Get("redirect_uri") != g.redirect:
		fail("invalid_grant")
	case b64(sum[:]) != g.challenge:
		fail("invalid_grant") // PKCE
	default:
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "at-" + randomString(8), "token_type": "Bearer", "expires_in": 3600, "id_token": f.sign(g.claims),
		})
	}
}
