package main

// How a phone reaches this server: the URLs listed by GET /api/joininfo, and
// QR codes for them (GET /api/joinqr.png). Recomputed on every request, since
// addresses change when the Mac joins a network or turns on Internet Sharing.

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	qrcode "github.com/skip2/go-qrcode"
)

// internetSharingIP is the address macOS gives the bridge when Internet
// Sharing is on. Phones joined to that hotspot reach the Mac here, so it is
// listed first.
var internetSharingIP = net.IPv4(192, 168, 2, 1).To4()

// joinSettings says where the listeners are, so the URLs can be built from the
// real ports. -public-url, when set, is listed first.
type joinSettings struct {
	public    *url.URL
	httpIP    net.IP // nil when bound to all interfaces
	httpPort  int    // 0 when there is no plain HTTP listener
	httpsIP   net.IP
	httpsPort int
}

// newJoinSettings checks the -public-url flag. An empty value is allowed.
func newJoinSettings(raw string) (*joinSettings, error) {
	j := &joinSettings{}
	if raw == "" {
		return j, nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("-public-url: %w", err)
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("-public-url must be an http:// or https:// URL with a host, got %q", raw)
	}
	u.RawQuery = ""
	u.Fragment = ""
	if u.Path == "" {
		u.Path = "/"
	}
	j.public = u
	return j, nil
}

// setListener records the address a listener actually bound to (so a port of
// 0 works too). Call it before the listener starts serving.
func (j *joinSettings) setListener(https bool, a net.Addr) {
	t, ok := a.(*net.TCPAddr)
	if !ok {
		return
	}
	ip := t.IP
	if ip.IsUnspecified() {
		ip = nil
	}
	if https {
		j.httpsIP, j.httpsPort = ip, t.Port
	} else {
		j.httpIP, j.httpPort = ip, t.Port
	}
}

// joinURL is one phone URL. Secure means https.
type joinURL struct {
	URL    string `json:"url"`
	Secure bool   `json:"secure"`
	Label  string `json:"label"`
}

type joinInfo struct {
	URLs []joinURL `json:"urls"`
}

// addrHost is a host part of a phone URL with a label for people to read.
type addrHost struct {
	name  string
	label string
}

// urls returns the current phone URLs: -public-url first, then https URLs,
// then plain http URLs. A nil receiver gives an empty list.
func (j *joinSettings) urls() []joinURL {
	if j == nil {
		return []joinURL{}
	}
	var https, plain []addrHost
	if j.httpsPort > 0 {
		https = j.hosts(j.httpsIP)
	}
	if j.httpPort > 0 {
		plain = j.hosts(j.httpIP)
	}
	return buildJoinURLs(j.public, https, j.httpsPort, plain, j.httpPort)
}

// buildJoinURLs is the pure part of urls(): it orders and de-duplicates the
// URLs for the given hosts and ports.
func buildJoinURLs(public *url.URL, https []addrHost, httpsPort int, plain []addrHost, httpPort int) []joinURL {
	out := []joinURL{}
	seen := map[string]bool{}
	add := func(u joinURL) {
		if !seen[u.URL] {
			seen[u.URL] = true
			out = append(out, u)
		}
	}
	if public != nil {
		add(joinURL{URL: public.String(), Secure: public.Scheme == "https", Label: "Preferred"})
	}
	if httpsPort > 0 {
		for _, h := range https {
			add(joinURL{URL: phoneURL("https", h.name, httpsPort), Secure: true, Label: h.label})
		}
	}
	if httpPort > 0 {
		for _, h := range plain {
			add(joinURL{URL: phoneURL("http", h.name, httpPort), Secure: false, Label: h.label})
		}
	}
	return out
}

func phoneURL(scheme, host string, port int) string {
	return scheme + "://" + net.JoinHostPort(host, strconv.Itoa(port)) + "/"
}

// hosts lists the host parts for a listener. A listener bound to one address
// gives only that address. Otherwise it gives the usable IPv4 addresses (the
// Internet Sharing one first), then the mDNS host name.
func (j *joinSettings) hosts(bound net.IP) []addrHost {
	if bound != nil {
		return []addrHost{{name: bound.String(), label: "Bound address"}}
	}
	var out []addrHost
	ips := lanIPv4s()
	for _, c := range ips {
		if c.ip.Equal(internetSharingIP) {
			out = append(out, addrHost{name: c.ip.String(), label: "Internet Sharing (" + c.iface + ")"})
		}
	}
	for _, c := range ips {
		if !c.ip.Equal(internetSharingIP) {
			out = append(out, addrHost{name: c.ip.String(), label: "Network (" + c.iface + ")"})
		}
	}
	if name := mdnsName(); name != "" {
		out = append(out, addrHost{name: name, label: "Bonjour name"})
	}
	return out
}

type ifaceIP struct {
	ip    net.IP
	iface string
}

// lanIPv4s returns the current usable IPv4 addresses on interfaces that are up.
func lanIPv4s() []ifaceIP {
	ifs, err := net.Interfaces()
	if err != nil {
		log.Printf("joininfo: interfaces: %v", err)
		return nil
	}
	var out []ifaceIP
	for _, ifc := range ifs {
		if ifc.Flags&net.FlagUp == 0 {
			continue
		}
		addrs, err := ifc.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			n, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			ip := n.IP.To4()
			if ip == nil || !usableLANIPv4(ip) {
				continue
			}
			out = append(out, ifaceIP{ip: ip, iface: ifc.Name})
		}
	}
	return out
}

var notLANIPv4 = []*net.IPNet{
	mustCIDR("127.0.0.0/8"),
	mustCIDR("169.254.0.0/16"), // link-local
	mustCIDR("100.64.0.0/10"),  // CGNAT, Tailscale
	mustCIDR("192.0.0.0/24"),   // IETF protocol assignments
}

// usableLANIPv4 reports whether a phone could plausibly reach this address.
func usableLANIPv4(ip net.IP) bool {
	ip = ip.To4()
	if ip == nil || ip.IsUnspecified() || ip.IsMulticast() {
		return false
	}
	for _, n := range notLANIPv4 {
		if n.Contains(ip) {
			return false
		}
	}
	return true
}

func mustCIDR(s string) *net.IPNet {
	_, n, err := net.ParseCIDR(s)
	if err != nil {
		panic(err)
	}
	return n
}

// mdnsName is the Bonjour host name for this Mac, always ending in .local.
// On macOS it is the LocalHostName (scutil), which is what Bonjour advertises.
// os.Hostname can differ (it is the computer name, e.g. "Example-MBP").
func mdnsName() string {
	return mdnsNameFrom(cachedLocalHostName, os.Hostname)
}

// mdnsNameFrom picks the Bonjour name: LocalHostName when it is usable, else
// the hostname. Both sources are injected so tests can run them.
func mdnsNameFrom(local func() string, hostname func() (string, error)) string {
	if n := mdnsFromHostname(local()); n != "" {
		return n
	}
	h, err := hostname()
	if err != nil {
		return ""
	}
	return mdnsFromHostname(h)
}

const (
	localHostNameTTL     = 60 * time.Second
	localHostNameTimeout = 1 * time.Second
)

var localHostNameCache struct {
	mu      sync.Mutex
	value   string
	fetched time.Time
	valid   bool
}

// readLocalHostName is localHostName, as a variable so tests can count calls.
var readLocalHostName = localHostName

// cachedLocalHostName returns localHostName() on macOS, remembered for
// localHostNameTTL. Elsewhere it returns "".
func cachedLocalHostName() string {
	c := &localHostNameCache
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.valid && time.Since(c.fetched) < localHostNameTTL {
		return c.value
	}
	c.value = readLocalHostName()
	c.fetched = time.Now()
	c.valid = true
	return c.value
}

// localHostName runs `scutil --get LocalHostName` (1 s timeout). It returns ""
// off macOS or when the command fails.
func localHostName() string {
	if runtime.GOOS != "darwin" {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), localHostNameTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "scutil", "--get", "LocalHostName").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func mdnsFromHostname(h string) string {
	h = strings.TrimSuffix(strings.TrimSpace(h), ".")
	if h == "" || strings.HasPrefix(h, ".") {
		return ""
	}
	if !strings.HasSuffix(strings.ToLower(h), ".local") {
		if i := strings.IndexByte(h, '.'); i >= 0 {
			h = h[:i]
		}
		h += ".local"
	}
	for _, r := range h {
		ok := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '.'
		if !ok {
			return ""
		}
	}
	return h
}

// handleJoinInfo serves GET /api/joininfo.
func (s *Server) handleJoinInfo(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeErr(w, http.StatusMethodNotAllowed, "GET only")
		return
	}
	writeJSON(w, http.StatusOK, joinInfo{URLs: s.joinCfg.urls()})
}

// handleJoinQR serves GET /api/joinqr.png?u=<url>. Only URLs listed by
// /api/joininfo right now get a code, so this is not an open QR generator.
func (s *Server) handleJoinQR(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeErr(w, http.StatusMethodNotAllowed, "GET only")
		return
	}
	target := r.URL.Query().Get("u")
	listed := false
	for _, u := range s.joinCfg.urls() {
		if u.URL == target {
			listed = true
			break
		}
	}
	if !listed {
		writeErr(w, http.StatusNotFound, "not a current join URL")
		return
	}
	png, err := qrcode.Encode(target, qrcode.Medium, 512)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "qr: "+err.Error())
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(png)
}
