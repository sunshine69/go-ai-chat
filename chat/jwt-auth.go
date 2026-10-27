package main

// Chat client's half of the JWT handshake with the MCP server.
//
// The server side (mcp-srv/jwt-auth.go) only verifies. This side mints the
// token and puts it on the wire:
//
//   -jwt-secret <secret>   same secret as the server's flag
//   aig -jwt-secret s /mcp http://host:8080/mcp/proj1
//                          → every HTTP MCP request carries
//                            Authorization: Bearer <jwt>
//
// The token is minted once per MCP connection, with sub = config.Model. The server
// reads 'sub' back out and uses it as the model name in its network audit log, which
// is the whole point: the MCP wire protocol has no field for "which model asked for
// this tool call", so the model name has to ride along in something the server will
// already accept, and Authorization is the one header the server sees.
//
// The token is minted per connection rather than once at startup on purpose:
// `/m <model>` changes config.Model mid-session, and a token minted at startup
// would keep reporting the old model for the rest of the run.

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// jwtSecret is the shared secret for this process; empty means no token is sent
// and the client behaves exactly as before.
var jwtSecret = ""

// takeFlag pulls a "-name value" or "-name=value" pair out of os.Args and returns
// its value. It removes the pair so the remaining arguments are still parsed
// correctly by handleNonInteractive's /command loop - the flag is a process
// setting, not a command, and must be read before any MCP connection is made.
func takeFlag(name string) string {
	args := os.Args
	for i, arg := range args {
		if arg == "-"+name {
			if i+1 >= len(args) {
				fmt.Fprintf(os.Stderr, "❌ -%s requires a value\n", name)
				os.Exit(1)
			}
			value := args[i+1]
			os.Args = append(args[:i], args[i+2:]...)
			return value
		}
		if strings.HasPrefix(arg, "-"+name+"=") {
			value := strings.TrimPrefix(arg, "-"+name+"=")
			os.Args = append(args[:i], args[i+1:]...)
			return value
		}
	}
	return ""
}

// jwtTokenFor mints the Authorization token for the current model.
func jwtTokenFor(model string) (string, error) {
	if jwtSecret == "" {
		return "", fmt.Errorf("no jwt secret configured")
	}
	now := time.Now()
	claims := jwt.MapClaims{}
	claims["sub"] = model
	// MarshalSingleStringAsArray is true here, so this serialises as ["aig"] —
	// the same value the server expects if it ever wants to pin an audience.
	claims["aud"] = []string{"aig"}
	claims["iat"] = jwt.NewNumericDate(now)
	// Far future on purpose: the token is minted once per MCP connection and has
	// to survive a long chat session. Expiry is not what protects this channel —
	// the shared secret is.
	claims["exp"] = jwt.NewNumericDate(now.Add(365 * 24 * time.Hour))
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	signed, err := token.SignedString([]byte(jwtSecret))
	if err != nil {
		return "", fmt.Errorf("jwt: %v", err)
	}
	return signed, nil
}
