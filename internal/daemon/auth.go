package daemon

import (
	"bufio"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"golang.org/x/crypto/md4"
)

// ErrAuthFailed is returned by ServeAuth and DialAuth when authentication
// is attempted and fails - a wrong password, an unauthorized user, or a
// secrets file that can't be read. The wire-level detail behind it is
// deliberately generic (see ServeAuth), matching real rsync's own refusal
// to distinguish "no such user" from "wrong password" in its response.
var ErrAuthFailed = errors.New("authentication failed")

// AuthRequired reports whether a client must authenticate to use m: real
// rsyncd.conf's own rule is that a module requires auth exactly when it
// has a non-empty "auth users" list.
func (m Module) AuthRequired() bool {
	return len(m.AuthUsers) > 0
}

// PasswordFunc resolves the password to use for daemon authentication.
// DialAuth calls it at most once, and only if the server actually
// challenges for a password (an "@RSYNCD: AUTHREQD" line) - matching real
// rsync's own client behavior, where auth_client() is only ever invoked
// in response to that same line. Connecting to a module that turns out
// not to require authentication never calls this at all, so a caller
// backing it with an interactive terminal prompt or a --password-file
// read never triggers either one against an anonymous module - resolving
// eagerly, before knowing whether the server will ask, would be a real
// regression from real rsync's behavior here, not just a style choice.
type PasswordFunc func() (string, error)

// StaticPassword wraps an already-known password (e.g. a test fixture, or
// a caller that has already decided eager resolution is fine) as a
// PasswordFunc.
func StaticPassword(password string) PasswordFunc {
	return func() (string, error) { return password, nil }
}

// md4Hash returns the base64-encoded (standard alphabet, no padding - the
// same encoding real rsync's own base64_encode(..., pad=0) produces) MD4
// digest of secret followed by challenge. This matches real rsync's
// generate_hash() in authenticate.c exactly: the secret is hashed first,
// then the challenge, with no seed byte - verified against the actual
// rsync source rather than assumed, since getting the byte order wrong
// here would silently produce a client and server that only interoperate
// with each other, never with real rsync or real docs describing the
// algorithm.
func md4Hash(secret, challenge string) string {
	h := md4.New()
	// hash.Hash.Write (which io.WriteString goes through) never returns
	// an error - its doc comment guarantees this - so there is nothing
	// meaningful to check here.
	_, _ = io.WriteString(h, secret)
	_, _ = io.WriteString(h, challenge)
	return base64.RawStdEncoding.EncodeToString(h.Sum(nil))
}

// generateChallenge returns a fresh, random, base64-encoded challenge.
// Real rsync derives its challenge from the client address, current time,
// and pid; grsync uses a CSPRNG instead, which is at least as
// unpredictable and far simpler - nothing in the protocol requires the
// challenge to be derived any particular way, only that it not repeat.
func generateChallenge() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generating challenge: %w", err)
	}
	return base64.RawStdEncoding.EncodeToString(buf), nil
}

// readSecretsFile parses a "name:secret" per-line secrets file, matching
// the real "secrets file" format. Blank lines and "#"-prefixed lines are
// skipped; a line with no ":" is skipped rather than treated as an error,
// since it can never match a submitted username anyway.
func readSecretsFile(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("opening secrets file: %w", err)
	}
	defer func() { _ = f.Close() }()

	secrets := make(map[string]string)
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name, secret, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		secrets[name] = secret
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("reading secrets file: %w", err)
	}
	return secrets, nil
}

func userAllowed(user string, allowed []string) bool {
	for _, u := range allowed {
		if u == user {
			return true
		}
	}
	return false
}

// ServeAuth runs the server side of module authentication, following
// ServeGreeting having already selected m. If m doesn't require auth, it
// writes "@RSYNCD: OK" and returns immediately with an empty user. Password
// comparison happens in constant time (crypto/subtle) so a wrong-length or
// wrong-content response can't be distinguished by timing; the refusal
// reason is likewise never revealed on the wire, matching real rsync's own
// single generic "@ERROR: auth failed on module <name>" message regardless
// of whether the username was unknown, unauthorized, or the password was
// wrong - only the server's own logs (not implemented here) would ever see
// that detail in real rsync.
func ServeAuth(c *conn, m Module) (user string, err error) {
	if !m.AuthRequired() {
		if err := writeLine(c.w, "@RSYNCD: OK"); err != nil {
			return "", fmt.Errorf("writing OK: %w", err)
		}
		return "", nil
	}

	challenge, err := generateChallenge()
	if err != nil {
		return "", err
	}
	if err := writeLine(c.w, "@RSYNCD: AUTHREQD "+challenge); err != nil {
		return "", fmt.Errorf("writing AUTHREQD: %w", err)
	}

	line, err := readLine(c.r)
	if err != nil {
		return "", fmt.Errorf("reading auth response: %w", err)
	}
	submittedUser, response, ok := strings.Cut(line, " ")
	if !ok {
		_ = writeLine(c.w, fmt.Sprintf("@ERROR: auth failed on module %s", m.Name))
		return "", fmt.Errorf("%w: malformed auth response", ErrAuthFailed)
	}

	fail := func() (string, error) {
		_ = writeLine(c.w, fmt.Sprintf("@ERROR: auth failed on module %s", m.Name))
		return "", ErrAuthFailed
	}

	if !userAllowed(submittedUser, m.AuthUsers) {
		return fail()
	}
	secrets, err := readSecretsFile(m.SecretsFile)
	if err != nil {
		return fail()
	}
	secret, ok := secrets[submittedUser]
	if !ok {
		return fail()
	}
	expected := md4Hash(secret, challenge)
	if subtle.ConstantTimeCompare([]byte(response), []byte(expected)) != 1 {
		return fail()
	}

	if err := writeLine(c.w, "@RSYNCD: OK"); err != nil {
		return "", fmt.Errorf("writing OK: %w", err)
	}
	return submittedUser, nil
}

// DialAuth runs the client side of authentication: reads lines until
// "@RSYNCD: OK", answering an "@RSYNCD: AUTHREQD <challenge>" line (if one
// arrives) with "<user> <md4Hash(password, challenge)>" - the password
// itself is never sent, only this one-way hash of it, so ErrAuthFailed
// and any wire capture of this exchange should never contain it. An empty
// user is sent as "nobody", matching real rsync's own client behavior for
// anonymous-looking auth attempts. password is only ever called if the
// server actually asks for one - see PasswordFunc's doc comment for why
// that laziness matters, not just how it works.
func DialAuth(c *conn, user string, password PasswordFunc) error {
	for {
		line, err := readLine(c.r)
		if err != nil {
			return fmt.Errorf("reading server response: %w", err)
		}

		const authReqdPrefix = "@RSYNCD: AUTHREQD "
		if strings.HasPrefix(line, authReqdPrefix) {
			challenge := strings.TrimPrefix(line, authReqdPrefix)
			sendUser := user
			if sendUser == "" {
				sendUser = "nobody"
			}
			pass, err := password()
			if err != nil {
				return fmt.Errorf("resolving password: %w", err)
			}
			response := md4Hash(pass, challenge)
			if err := writeLine(c.w, sendUser+" "+response); err != nil {
				return fmt.Errorf("writing auth response: %w", err)
			}
			continue
		}

		if line == "@RSYNCD: OK" {
			return nil
		}
		if strings.HasPrefix(line, "@ERROR") {
			return fmt.Errorf("%w: %s", ErrAuthFailed, line)
		}
		return fmt.Errorf("unexpected line during authentication: %q", line)
	}
}
