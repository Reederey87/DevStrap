package daemon

import (
	"context"
	"fmt"
	"net"
	"net/http"

	"github.com/Reederey87/DevStrap/internal/platform"
)

// socketHost is the fixed Host header value clients send. Requests carrying any
// other host are refused (see guard). Over a Unix socket the host is
// meaningless for routing, so pinning it costs nothing and blocks a class of
// confused-deputy requests aimed at a loopback listener.
const socketHost = "devstrapd.sock"

// maxRequestBody bounds every request body. The API's largest legitimate
// request is a small JSON object.
const maxRequestBody = 1 << 20 // 1 MiB

type peerContextKey struct{}

// peerAuth carries the connection's resolved identity, or the reason it could
// not be resolved. It is attached at CONNECTION time (ConnContext) rather than
// per request, because the identity is a property of the socket, and resolving
// it once per connection means a request cannot be served before the check has
// happened.
type peerAuth struct {
	identity platform.PeerIdentity
	err      error
}

// connContext resolves peer credentials for each accepted connection and stores
// the result in the connection's context. A failure is recorded rather than
// returned: net/http has no way to reject a connection from this hook, so the
// error is carried forward and the guard turns it into a 403 — the connection
// is accepted at the socket layer but can never be served.
func connContext(ctx context.Context, conn net.Conn) context.Context {
	unixConn, ok := conn.(*net.UnixConn)
	if !ok {
		return context.WithValue(ctx, peerContextKey{}, peerAuth{
			err: fmt.Errorf("daemon: connection is not a unix socket (%T)", conn),
		})
	}
	identity, err := platform.PeerCred(unixConn)
	return context.WithValue(ctx, peerContextKey{}, peerAuth{identity: identity, err: err})
}

func peerFromContext(ctx context.Context) (peerAuth, bool) {
	auth, ok := ctx.Value(peerContextKey{}).(peerAuth)
	return auth, ok
}

// authorizePeer decides whether a resolved peer may drive this daemon. It is
// split out from the middleware so the decision is unit-testable for uids the
// test process cannot actually connect as (notably root).
//
// The rule is deliberately narrow: the peer must be the same uid the daemon
// runs as. Root is NOT exempt — a root process can open a 0600 socket owned by
// another user, so exempting it would defeat the only control that still
// applies once filesystem permissions have been bypassed.
func authorizePeer(auth peerAuth, serverUID uint32) error {
	if auth.err != nil {
		return fmt.Errorf("peer identity unavailable: %w", auth.err)
	}
	if auth.identity.UID != serverUID {
		return fmt.Errorf("peer uid %d is not the daemon's uid %d", auth.identity.UID, serverUID)
	}
	return nil
}

// guard wraps every route with the connection- and request-level checks.
func (s *Server) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set(versionHeader, s.version)

		auth, ok := peerFromContext(r.Context())
		if !ok {
			// No hook ran for this connection: fail closed rather than assume.
			writeError(w, http.StatusForbidden, "peer identity was not resolved")
			return
		}
		if err := authorizePeer(auth, s.uid); err != nil {
			s.logger.Warn("daemon: refused connection", "reason", err.Error())
			writeError(w, http.StatusForbidden, "forbidden")
			return
		}

		// A local API must never be drivable by a web page the user happens to
		// have open. Browsers always attach one of these on a cross-origin
		// request, and no legitimate DevStrap client sends either.
		if r.Header.Get("Origin") != "" || r.Header.Get("Referer") != "" {
			writeError(w, http.StatusForbidden, "cross-origin requests are not accepted")
			return
		}
		if r.Host != "" && r.Host != socketHost {
			writeError(w, http.StatusForbidden, "unexpected host")
			return
		}

		r.Body = http.MaxBytesReader(w, r.Body, maxRequestBody)
		next.ServeHTTP(w, r)
	})
}
