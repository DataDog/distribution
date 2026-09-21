package token

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/distribution/distribution/v3/registry/auth"
	"github.com/go-jose/go-jose/v4"
	"github.com/sirupsen/logrus"
)

func TestBuildAutoRedirectURL(t *testing.T) {
	cases := []struct {
		name             string
		reqGetter        func() *http.Request
		autoRedirectPath string
		expectedURL      string
	}{{
		name: "http",
		reqGetter: func() *http.Request {
			req := httptest.NewRequest("GET", "http://example.com/", nil)
			return req
		},
		autoRedirectPath: "/auth",
		expectedURL:      "https://example.com/auth",
	}, {
		name: "x-forwarded",
		reqGetter: func() *http.Request {
			req := httptest.NewRequest("GET", "http://example.com/", nil)
			req.Header.Set("X-Forwarded-Proto", "http")
			return req
		},
		autoRedirectPath: "/auth/token",
		expectedURL:      "http://example.com/auth/token",
	}}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := tc.reqGetter()
			result := buildAutoRedirectURL(req, tc.autoRedirectPath)
			if result != tc.expectedURL {
				t.Errorf("expected %s, got %s", tc.expectedURL, result)
			}
		})
	}
}

func TestCheckOptions(t *testing.T) {
	realm := "https://auth.example.com/token/"
	issuer := "test-issuer.example.com"
	service := "test-service.example.com"

	options := map[string]any{
		"realm":            realm,
		"issuer":           issuer,
		"service":          service,
		"rootcertbundle":   "",
		"autoredirect":     true,
		"autoredirectpath": "/auth",
	}

	ta, err := checkOptions(options)
	if err != nil {
		t.Fatal(err)
	}
	if ta.autoRedirect != true {
		t.Fatal("autoredirect should be true")
	}
	if ta.autoRedirectPath != "/auth" {
		t.Fatal("autoredirectpath should be /auth")
	}

	options = map[string]any{
		"realm":                        realm,
		"issuer":                       issuer,
		"service":                      service,
		"rootcertbundle":               "",
		"autoredirect":                 true,
		"autoredirectforcetlsdisabled": true,
	}

	ta, err = checkOptions(options)
	if err != nil {
		t.Fatal(err)
	}
	if ta.autoRedirect != true {
		t.Fatal("autoredirect should be true")
	}
	if ta.autoRedirectPath != "/auth/token" {
		t.Fatal("autoredirectpath should be /auth/token")
	}
}

func mockGetRootCerts(path string) ([]*x509.Certificate, error) {
	caPrivKey, err := rsa.GenerateKey(rand.Reader, 1024) // not to slow down the test that much
	if err != nil {
		return nil, err
	}

	ca := &x509.Certificate{
		PublicKey: &caPrivKey.PublicKey,
	}

	return []*x509.Certificate{ca}, nil
}

func mockGetJwks(path string) (*jose.JSONWebKeySet, error) {
	return &jose.JSONWebKeySet{
		Keys: []jose.JSONWebKey{
			{
				KeyID: "sample-key-id",
			},
		},
	}, nil
}

func TestCheckOptionsInvalidJWKSURL(t *testing.T) {
	base := map[string]any{
		"realm":   "https://auth.example.com/token/",
		"issuer":  "test-issuer.example.com",
		"service": "test-service.example.com",
	}

	cases := []struct {
		name string
		jwks string
	}{
		{"no host", "https://"},
		{"invalid url", "https://[::1]invalid"},
		{"ftp scheme", "ftp://auth.example.com/jwks.json"},
		{"ssh scheme", "ssh://auth.example.com/jwks.json"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			opts := make(map[string]any, len(base)+1)
			maps.Copy(opts, base)
			opts["jwks"] = tc.jwks

			if _, err := checkOptions(opts); err == nil {
				t.Fatalf("expected error for jwks=%q, got nil", tc.jwks)
			}
		})
	}
}

func TestCheckOptionsValidJWKSURL(t *testing.T) {
	cases := []string{
		"https://auth.example.com/.well-known/jwks.json",
		"http://localhost:8080/jwks",
	}

	base := map[string]any{
		"realm":   "https://auth.example.com/token/",
		"issuer":  "test-issuer.example.com",
		"service": "test-service.example.com",
	}

	for _, jwks := range cases {
		t.Run(jwks, func(t *testing.T) {
			opts := make(map[string]any, len(base)+1)
			maps.Copy(opts, base)
			opts["jwks"] = jwks

			ta, err := checkOptions(opts)
			if err != nil {
				t.Fatalf("unexpected error for jwks=%q: %v", jwks, err)
			}
			if ta.jwks != jwks {
				t.Fatalf("expected jwks=%q, got %q", jwks, ta.jwks)
			}
		})
	}
}

func TestRootCertIncludedInTrustedKeys(t *testing.T) {
	old := rootCertFetcher
	rootCertFetcher = mockGetRootCerts
	defer func() { rootCertFetcher = old }()

	realm := "https://auth.example.com/token/"
	issuer := "test-issuer.example.com"
	service := "test-service.example.com"

	options := map[string]any{
		"realm":            realm,
		"issuer":           issuer,
		"service":          service,
		"rootcertbundle":   "something-to-trigger-our-mock",
		"autoredirect":     true,
		"autoredirectpath": "/auth",
	}

	ac, err := newAccessController(options)
	if err != nil {
		t.Fatal(err)
	}
	// newAccessController return type is an interface built from
	// accessController struct. The type check can be safely ignored.
	ac2, _ := ac.(*accessController)
	if got := len(ac2.trustedKeys); got != 1 {
		t.Fatalf("Unexpected number of trusted keys, expected 1 got: %d", got)
	}
}

func TestJWKSIncludedInTrustedKeys(t *testing.T) {
	old := jwkFetcher
	jwkFetcher = mockGetJwks
	defer func() { jwkFetcher = old }()

	realm := "https://auth.example.com/token/"
	issuer := "test-issuer.example.com"
	service := "test-service.example.com"

	options := map[string]any{
		"realm":            realm,
		"issuer":           issuer,
		"service":          service,
		"jwks":             "something-to-trigger-our-mock",
		"autoredirect":     true,
		"autoredirectpath": "/auth",
	}

	ac, err := newAccessController(options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ac.(*accessController).Close() })

	// newAccessController return type is an interface built from
	// accessController struct. The type check can be safely ignored.
	ac2, _ := ac.(*accessController)
	if got := len(ac2.trustedKeys); got != 1 {
		t.Fatalf("Unexpected number of trusted keys, expected 1 got: %d", got)
	}
}

func TestGetJWKSFromURL(t *testing.T) {
	// Hardcoded JWKS JSON to avoid marshaling issues with nil key material.
	const body = `{"keys":[{"kty":"oct","kid":"key-from-url","k":"c2VjcmV0"}]}`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	got, err := getJwks(srv.URL)
	if err != nil {
		t.Fatalf("getJwks from URL: %v", err)
	}
	if len(got.Keys) != 1 || got.Keys[0].KeyID != "key-from-url" {
		t.Fatalf("unexpected keys: %+v", got.Keys)
	}
}

func TestGetJWKSFromURLNon200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer srv.Close()

	if _, err := getJwks(srv.URL); err == nil {
		t.Fatal("expected error for non-200 response, got nil")
	}
}

func TestJWKSRefresh(t *testing.T) {
	originalLevel := logrus.GetLevel()
	t.Cleanup(func() { logrus.SetLevel(originalLevel) })
	logrus.SetLevel(logrus.FatalLevel)
	const (
		initialJWKS = `{"keys":[{"kty":"oct","kid":"initial-key","k":"c2VjcmV0"}]}`
		rotatedJWKS = `{"keys":[{"kty":"oct","kid":"rotated-key","k":"c2VjcmV0"}]}`
	)

	var callCount int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		body := initialJWKS
		if callCount > 1 {
			body = rotatedJWKS
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	options := map[string]any{
		"realm":               "https://auth.example.com/token/",
		"issuer":              "test-issuer.example.com",
		"service":             "test-service.example.com",
		"jwks":                srv.URL,
		"jwksrefreshinterval": "100ms",
	}

	ac, err := newAccessController(options)
	if err != nil {
		t.Fatal(err)
	}
	ac2 := ac.(*accessController)
	defer ac2.Close()

	// Wait for at least one refresh cycle.
	time.Sleep(200 * time.Millisecond)

	ac2.mu.RLock()
	_, hasRotated := ac2.trustedKeys["rotated-key"]
	ac2.mu.RUnlock()

	if !hasRotated {
		t.Fatalf("expected trustedKeys to contain %q after refresh, got: %v", "rotated-key", ac2.trustedKeys)
	}
}

func TestJWKSRefreshKeepsOldKeysOnError(t *testing.T) {
	originalLevel := logrus.GetLevel()
	t.Cleanup(func() { logrus.SetLevel(originalLevel) })
	logrus.SetLevel(logrus.FatalLevel)
	const initialJWKS = `{"keys":[{"kty":"oct","kid":"original-key","k":"c2VjcmV0"}]}`

	var callCount int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		if callCount > 1 {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(initialJWKS))
	}))
	defer srv.Close()

	options := map[string]any{
		"realm":               "https://auth.example.com/token/",
		"issuer":              "test-issuer.example.com",
		"service":             "test-service.example.com",
		"jwks":                srv.URL,
		"jwksrefreshinterval": "100ms",
	}

	ac, err := newAccessController(options)
	if err != nil {
		t.Fatal(err)
	}
	ac2 := ac.(*accessController)
	defer ac2.Close()

	// Wait for a failed refresh cycle.
	time.Sleep(200 * time.Millisecond)

	ac2.mu.RLock()
	_, hasOriginal := ac2.trustedKeys["original-key"]
	ac2.mu.RUnlock()

	if !hasOriginal {
		t.Fatalf("expected original key to be preserved after failed refresh, got: %v", ac2.trustedKeys)
	}
}

// TestAuthorizedOnDemandJWKSRefreshGuard covers the amplification guard on the
// on-demand JWKS refresh: concurrent unknown-kid requests must trigger exactly
// one fetch, a burst arriving within the minimum interval must trigger none,
// and a request outside the interval must trigger a new one.
func TestAuthorizedOnDemandJWKSRefreshGuard(t *testing.T) {
	const (
		issuer  = "test-issuer.example.com"
		service = "test-service.example.com"
	)

	originalLevel := logrus.GetLevel()
	t.Cleanup(func() { logrus.SetLevel(originalLevel) })
	logrus.SetLevel(logrus.FatalLevel)

	keys, err := makeRootKeys(2)
	if err != nil {
		t.Fatal(err)
	}
	keyAID := keys[0].X.String()
	keyBID := keys[1].X.String()

	// The server always serves key A only, so key B remains unknown: every
	// request below is a rejected unknown-kid request, exactly like an
	// attacker sending garbage-kid tokens.
	var fetches atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fetches.Add(1)
		// Keep fetches slow enough that concurrent requests overlap and
		// exercise the singleflight deduplication path.
		time.Sleep(25 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{
			{Key: &keys[0].PublicKey, KeyID: keyAID, Algorithm: string(jose.ES256)},
		}})
	}))
	t.Cleanup(srv.Close)

	options := map[string]any{
		"realm":               "https://auth.example.com/token/",
		"issuer":              issuer,
		"service":             service,
		"jwks":                srv.URL,
		"jwksrefreshinterval": "0s", // disable periodic refresh; only on-demand
	}

	ac, err := newAccessController(options)
	if err != nil {
		t.Fatal(err)
	}
	ac2 := ac.(*accessController)
	t.Cleanup(func() { _ = ac2.Close() })
	// Shrink the on-demand minimum interval so the test does not have to
	// wait for the production default (5s).
	ac2.onDemandRefreshMinInterval = 200 * time.Millisecond

	// Token signed with key B, whose kid is never served: every request
	// below must be rejected.
	tokenB, err := makeTestTokenKIDOnly(
		keys[1], keyBID, issuer, service,
		[]*ResourceActions{{Type: "repository", Name: "foo/bar", Actions: []string{"pull"}}},
		time.Now(), time.Now().Add(5*time.Minute),
	)
	if err != nil {
		t.Fatal(err)
	}

	access := auth.Access{
		Resource: auth.Resource{Type: "repository", Name: "foo/bar"},
		Action:   "pull",
	}
	authorized := func() error {
		req, err := http.NewRequest(http.MethodGet, "http://example.com/v2/foo/bar/manifests/latest", nil)
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", tokenB.Raw))
		_, err = ac.Authorized(req, access)
		return err
	}

	// Initial JWKS load at controller construction.
	if got := fetches.Load(); got != 1 {
		t.Fatalf("expected 1 fetch after init, got %d", got)
	}

	// Burst 1: concurrent unknown-kid requests trigger exactly one fetch.
	concurrentRequests(t, 8, authorized)
	if got := fetches.Load(); got != 2 {
		t.Fatalf("expected exactly one on-demand fetch for a concurrent burst, got %d fetches total", got)
	}

	// Burst 2, immediately after: no fetch within the minimum interval;
	// requests are rejected against the current keys.
	concurrentRequests(t, 8, authorized)
	if got := fetches.Load(); got != 2 {
		t.Fatalf("expected no fetch within the minimum interval, got %d fetches total", got)
	}

	// Outside the minimum interval, the next unknown-kid request fetches again.
	time.Sleep(250 * time.Millisecond)
	if err := authorized(); err == nil {
		t.Fatal("expected request to be rejected, key B is never served")
	}
	if got := fetches.Load(); got != 3 {
		t.Fatalf("expected a new fetch outside the minimum interval, got %d fetches total", got)
	}
}

// concurrentRequests runs fn from n goroutines released at the same time and
// asserts every call returns a non-nil error (a challenge).
func concurrentRequests(t *testing.T, n int, fn func() error) {
	t.Helper()

	start := make(chan struct{})
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			errs[i] = fn()
		}()
	}
	close(start)
	wg.Wait()

	for i, err := range errs {
		if err == nil {
			t.Fatalf("request %d: expected rejection, got authorized", i)
		}
	}
}
