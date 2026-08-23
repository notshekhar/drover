package server

import (
	"net/http"

	"github.com/notshekhar/drover/internal/httpguard"
)

// guard wraps every route, including /mcp.
//
// The MCP endpoint has checked Origin since it shipped; the REST API never
// did, and it is the surface with `apply` on it. A page you visited could
// issue a simple cross-origin POST -- a form with enctype="text/plain",
// carrying a body that happens to be JSON -- to /apis/drover/v1/apply and
// register a Repository in your engine. It could not read the reply, so it was
// blind, but it was a write, and there was nothing between it and the store.
//
// Both halves are load-bearing and neither is sufficient alone. The
// content-type rule is what forces a browser to preflight; the Origin rule is
// what makes it fail. Drop the first and the attack never asks permission;
// drop the second and asking is granted.
func guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Nothing here is a document a browser should ever sniff a type for.
		w.Header().Set("X-Content-Type-Options", "nosniff")

		if !httpguard.OriginAllowed(r) {
			http.Error(w, "origin not allowed", http.StatusForbidden)
			return
		}
		if httpguard.HasBody(r.Method) && !httpguard.IsJSON(r) {
			http.Error(w, "this endpoint takes a JSON body; set Content-Type: application/json", http.StatusUnsupportedMediaType)
			return
		}
		next.ServeHTTP(w, r)
	})
}
