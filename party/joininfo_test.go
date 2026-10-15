package main

import (
	"bytes"
	"image/png"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"testing"
	"time"

	qrcode "github.com/skip2/go-qrcode"
)

func TestUsableLANIPv4(t *testing.T) {
	cases := map[string]bool{
		"192.168.0.23": true,
		"192.168.2.1":  true,
		"10.0.0.5":     true,
		"203.0.113.9":  true,
		"127.0.0.1":    false,
		"169.254.10.1": false,
		"100.126.1.2":  false, // Tailscale (CGNAT)
		"100.64.0.1":   false,
		"192.0.0.7":    false,
		"0.0.0.0":      false,
		"224.0.0.251":  false,
	}
	for s, want := range cases {
		if got := usableLANIPv4(net.ParseIP(s)); got != want {
			t.Errorf("usableLANIPv4(%s) = %v, want %v", s, got, want)
		}
	}
}

func TestMDNSFromHostname(t *testing.T) {
	cases := map[string]string{
		"Roberts-MacBook-Pro.local": "Roberts-MacBook-Pro.local",
		"Roberts-MBP":               "Roberts-MBP.local",
		"Roberts-MBP.":              "Roberts-MBP.local",
		"box.lan":                   "box.local",
		"my mac":                    "",
		"":                          "",
		".local":                    "",
	}
	for in, want := range cases {
		if got := mdnsFromHostname(in); got != want {
			t.Errorf("mdnsFromHostname(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestMDNSNameUsesLocalHostName(t *testing.T) {
	host := func(h string) func() (string, error) {
		return func() (string, error) { return h, nil }
	}
	failing := func() (string, error) { return "", os.ErrNotExist }
	cases := []struct {
		name  string
		local string
		host  func() (string, error)
		want  string
	}{
		// Bonjour (scutil LocalHostName) wins over the computer name.
		{"LocalHostName", "Example-MacBook-Pro", host("Example-MBP"), "Example-MacBook-Pro.local"},
		// No LocalHostName (scutil failed or not macOS): use os.Hostname.
		{"fallback to hostname", "", host("Example-MBP.lan"), "Example-MBP.local"},
		// A LocalHostName that is not a valid label is ignored.
		{"bad LocalHostName", "my mac", host("Example-MBP"), "Example-MBP.local"},
		{"nothing usable", "", failing, ""},
	}
	for _, c := range cases {
		local := func() string { return c.local }
		if got := mdnsNameFrom(local, c.host); got != c.want {
			t.Errorf("%s: mdnsNameFrom = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestLocalHostNameCached(t *testing.T) {
	calls := 0
	orig := readLocalHostName
	readLocalHostName = func() string { calls++; return "Example-MacBook-Pro" }
	localHostNameCache.valid = false
	defer func() {
		readLocalHostName = orig
		localHostNameCache.valid = false
	}()
	for i := 0; i < 3; i++ {
		if got := cachedLocalHostName(); got != "Example-MacBook-Pro" {
			t.Fatalf("cachedLocalHostName = %q", got)
		}
	}
	if calls != 1 {
		t.Errorf("scutil ran %d times in a row, want 1 (cached for 60 s)", calls)
	}
	localHostNameCache.fetched = time.Now().Add(-localHostNameTTL - time.Second)
	_ = cachedLocalHostName()
	if calls != 2 {
		t.Errorf("after the TTL scutil ran %d times, want 2", calls)
	}
}

func TestBuildJoinURLsOrder(t *testing.T) {
	public, _ := url.Parse("https://party.example:9443/")
	https := []addrHost{{"192.168.2.1", "Internet Sharing (bridge100)"}, {"192.168.0.5", "Network (en0)"}, {"mac.local", "Bonjour name"}}
	plain := []addrHost{{"192.168.2.1", "Internet Sharing (bridge100)"}, {"192.168.0.5", "Network (en0)"}}
	got := buildJoinURLs(public, https, 8443, plain, 8080)
	want := []joinURL{
		{"https://party.example:9443/", true, "Preferred"},
		{"https://192.168.2.1:8443/", true, "Internet Sharing (bridge100)"},
		{"https://192.168.0.5:8443/", true, "Network (en0)"},
		{"https://mac.local:8443/", true, "Bonjour name"},
		{"http://192.168.2.1:8080/", false, "Internet Sharing (bridge100)"},
		{"http://192.168.0.5:8080/", false, "Network (en0)"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d urls, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("url %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestBuildJoinURLsNoHTTPSAndDedupe(t *testing.T) {
	public, _ := url.Parse("http://192.168.0.5:8080/")
	plain := []addrHost{{"192.168.0.5", "Network (en0)"}}
	got := buildJoinURLs(public, nil, 0, plain, 8080)
	if len(got) != 1 || got[0].URL != "http://192.168.0.5:8080/" || got[0].Secure {
		t.Fatalf("got %+v, want the single preferred http URL", got)
	}
	if empty := buildJoinURLs(nil, nil, 0, nil, 0); empty == nil || len(empty) != 0 {
		t.Fatalf("empty build = %#v, want non-nil empty slice", empty)
	}
}

func TestNewJoinSettingsRejectsBadPublicURL(t *testing.T) {
	for _, bad := range []string{"ftp://x/", "party.local:8443", "https://"} {
		if _, err := newJoinSettings(bad); err == nil {
			t.Errorf("newJoinSettings(%q) accepted, want error", bad)
		}
	}
	j, err := newJoinSettings("https://party.local:8443?x=1")
	if err != nil || j.public.String() != "https://party.local:8443/" {
		t.Errorf("newJoinSettings normalise: %v, %v", j, err)
	}
}

func TestJoinQRRefusesUnlistedURL(t *testing.T) {
	s := &Server{joinCfg: &joinSettings{}}
	r := httptest.NewRequest(http.MethodGet, "/api/joinqr.png?u=https://evil.example/", nil)
	w := httptest.NewRecorder()
	s.handleJoinQR(w, r)
	if w.Code != http.StatusNotFound {
		t.Errorf("unlisted URL got %d, want 404", w.Code)
	}
}

func TestQREncodesPNG(t *testing.T) {
	b, err := qrcode.Encode("https://192.168.2.1:8443/", qrcode.Medium, 512)
	if err != nil {
		t.Fatal(err)
	}
	img, err := png.Decode(bytes.NewReader(b))
	if err != nil {
		t.Fatalf("not a PNG: %v", err)
	}
	if img.Bounds().Dx() < 256 {
		t.Errorf("QR image too small: %v", img.Bounds())
	}
}
