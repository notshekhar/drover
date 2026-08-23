// Package httpguard holds the browser defence drover's HTTP surfaces share.
//
// drover has no authentication by design: it listens on 127.0.0.1, so only
// that machine can reach it. That is a fine answer for another process on the
// machine and a bad one for a web page, because a page you visit runs on your
// machine too and can POST to a localhost port. These two checks are the whole
// defence against that, so they live in one place and both surfaces -- the
// MCP endpoint and the REST API -- apply the same rule.
package httpguard

import (
	"mime"
	"net"
	"net/http"
	"net/url"
	"strings"
)

// OriginAllowed reports whether a request may proceed.
//
// A request with no Origin is a direct client -- curl, an agent's HTTP client,
// the CLI -- and is allowed. A request that carries one is a browser, and its
// origin must be loopback, which is the only place a legitimate local client
// page could be served from. This is the DNS-rebinding defence the MCP
// transport spec calls out for local servers.
func OriginAllowed(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	return isLoopbackHost(u.Hostname())
}

func isLoopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// HasBody reports whether the method is one that carries a request body, and
// so is one the content-type rule applies to.
func HasBody(method string) bool {
	switch method {
	case http.MethodPost, http.MethodPut, http.MethodPatch:
		return true
	}
	return false
}

// IsJSON reports whether the request declares a JSON body.
//
// Requiring this is not pedantry about correctness, it is the second half of
// the CSRF defence. A cross-origin `fetch` or form post can only avoid a
// preflight by using a "simple" content type -- text/plain,
// application/x-www-form-urlencoded, multipart/form-data -- and a form with
// enctype="text/plain" will happily send a body that is valid JSON. Demanding
// application/json forces the browser to preflight, and the preflight is what
// the Origin check above then refuses. Without this rule the Origin check
// never runs on the attack that matters, because the attacking page never has
// to ask permission.
//
// Parameters are allowed, so "application/json; charset=utf-8" passes.
func IsJSON(r *http.Request) bool {
	ct := r.Header.Get("Content-Type")
	if ct == "" {
		return false
	}
	media, _, err := mime.ParseMediaType(ct)
	if err != nil {
		return false
	}
	media = strings.ToLower(media)
	return media == "application/json" || strings.HasSuffix(media, "+json")
}
