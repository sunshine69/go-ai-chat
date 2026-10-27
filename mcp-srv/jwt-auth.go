package main

// JWT authentication for the streamable HTTP transport, plus the model-name
// recovery that the network audit log needs.
//
// One shared secret, passed with -jwt-secret to both binaries:
//
//   chat client   signs a token with sub = config.Model and sends it as
//                Authorization: Bearer <token> on every HTTP MCP request
//   mcp server    verifies that token on the streamable endpoint, then records
//                sub so any later tool call in that session can report which
//                model issued it
//
// Why the HTTP layer and not the tool handler: the MCP wire protocol carries no
// model name at all, and mcp-go's streamable handler builds the tool-call context
// from a session id and throws the raw HTTP request away, so a handler can never
// read the Authorization header itself. The HTTP layer is the only place that
// sees the header, and it copies the model name back onto the request context
// which the handler does receive.
//
// Stdio transport is never authenticated - there is no header to check - so
// there the audit log keeps falling back to AIG_MODEL / "unknownmodel".

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// jwtModelKey stashes the authenticated model name on the request context, next
// to the "workdir" key the workdir manager already injects.
const jwtModelKey = "aig.model"

// jwtSecret is this process's shared secret; empty means authentication is off,
// which is exactly what -jwt-secret unset means.
var jwtSecret = ""

func setJWTSecret(secret string) { jwtSecret = secret }

// jwtTimeLeeway absorbs clock skew between the client that minted the token and
// this server.
const jwtTimeLeeway = 60 * time.Second

// jwtShape: a JWT is exactly three base64url segments. Rejecting anything else
// first keeps garbage out of the signature check.
var jwtShape = regexp.MustCompile(`^[A-Za-z0-9_\-]+\.[A-Za-z0-9_\-]+\.[A-Za-z0-9_\-]+$`)

// ---------------------------------------------------------------------------
// Verify / sign
// ---------------------------------------------------------------------------

// verifyJWT verifies an HS256 token and returns its claims.
//
// WithValidMethods is load-bearing, not decoration: omit it and a token claiming
// alg=none (or an asymmetric alg verified against an HMAC secret) is accepted.
// That is the textbook JWT algorithm-confusion attack.
func verifyJWT(token string) (jwt.MapClaims, error) {
	if !jwtShape.MatchString(token) {
		return nil, fmt.Errorf("malformed token")
	}
	if jwtSecret == "" {
		return nil, fmt.Errorf("no jwt secret configured")
	}
	claims := jwt.MapClaims{}
	// The KeyFunc ignores the token: WithValidMethods has already pinned the
	// algorithm, so there is no per-token key negotiation left to get wrong.
	parsed, err := jwt.ParseWithClaims(
		token,
		claims,
		func(_ *jwt.Token) (any, error) { return []byte(jwtSecret), nil },
		jwt.WithValidMethods([]string{"HS256"}),
		jwt.WithLeeway(jwtTimeLeeway),
	)
	if err != nil {
		return nil, fmt.Errorf("jwt: %v", err)
	}
	// ParseWithClaims is not enough on its own — a token can parse cleanly and
	// still fail signature or claim validation, which lands in parsed.Valid.
	if parsed == nil || !parsed.Valid {
		return nil, fmt.Errorf("jwt: token not valid")
	}
	return claims, nil
}

// signJWT mints an HS256 token for one model name - the chat client's half of
// the handshake. exp is far in the future deliberately: the token is minted once
// at MCP connect time and has to outlive the chat session, and it is the shared
// secret, not the expiry, that protects this channel.
func signJWT(model string) (string, error) {
	if jwtSecret == "" {
		return "", fmt.Errorf("no jwt secret configured")
	}
	now := time.Now()
	claims := jwt.MapClaims{}
	// MarshalSingleStringAsArray is true here, so this serialises as ["aig"].
	claims["aud"] = []string{"aig"}
	claims["iat"] = jwt.NewNumericDate(now)
	claims["exp"] = jwt.NewNumericDate(now.Add(365 * 24 * time.Hour))
	claims["sub"] = model
	signed, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte(jwtSecret))
	if err != nil {
		return "", fmt.Errorf("jwt: %v", err)
	}
	return signed, nil
}

// modelFromJWT returns the model name from the verified token's 'sub' claim.
// "" means "no usable token"; "unknownmodel" means "authenticated, no sub".
func modelFromJWT(token string) string {
	claims, err := verifyJWT(token)
	if err != nil {
		return ""
	}
	sub, _ := claims["sub"].(string)
	sub = strings.TrimSpace(sub)
	if sub == "" {
		return "unknownmodel"
	}
	return sub
}

// modelFromAuthorization verifies an Authorization header value.
func modelFromAuthorization(header string) string {
	token := strings.TrimSpace(header)
	// Scheme is case-insensitive per RFC 7235, and clients that omit it entirely
	// are common enough that accepting a bare token is worth the small looseness.
	if len(token) >= 7 && strings.EqualFold(token[:7], "bearer ") {
		token = strings.TrimSpace(token[7:])
	}
	return modelFromJWT(token)
}

// ---------------------------------------------------------------------------
// Audit-log lookup
// ---------------------------------------------------------------------------

// logModel picks the model name for one audit record, most specific first:
// the 'sub' of the token on this very request, else the AIG_MODEL default.
// AIG_MODEL stays the fallback because a streamable client that authenticates
// but sets no env var still gets a real name from the token, while a stdio
// session gets the env var or "unknownmodel" as before.
func logModel(ctx context.Context) string {
	fromToken, _ := ctx.Value(jwtModelKey).(string)
	if fromToken != "" {
		return fromToken
	}
	return networkLogModel
}

// ---------------------------------------------------------------------------
// HTTP auth middleware
// ---------------------------------------------------------------------------

// jwtGuard refuses everything unless the request carried a valid token, and
// injects the verified model name into the request context. Installed only for
// the streamable transport, and only when -jwt-secret is set.
func jwtGuard(inner http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// A CORS preflight never carries Authorization - browsers strip it from
		// OPTIONS - so it is answered by the inner handler unchanged. The real
		// request that follows is still authenticated.
		if r.Method == http.MethodOptions {
			inner.ServeHTTP(w, r)
			return
		}

		model := modelFromAuthorization(r.Header.Get("Authorization"))
		if model == "" {
			// One reply for "absent", "malformed" and "bad signature": distinguishing
			// them would only hand out an oracle.
			w.Header().Set("WWW-Authenticate", "Bearer realm=\"mcp\"")
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"jsonrpc":"2.0","error":{"code":-1,"message":"unauthorized"}}`))
			fmt.Fprintf(os.Stderr, "[AUTH] 401 %s %s auth=%s\n", r.Method, r.URL.Path, redact(r.Header.Get("Authorization")))
			return
		}

		inner.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), jwtModelKey, model)))
	})
}

// redact keeps tokens out of the server log while still showing whether one was
// sent at all.
func redact(header string) string {
	if header == "" {
		return "none"
	}
	return fmt.Sprintf("<%d bytes>", len(header))
}
