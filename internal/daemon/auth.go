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

// ErrAuthFailed is returned by ServeAuth and DialAuth when authentication fails.
var ErrAuthFailed = errors.New("authentication failed")

// AuthRequired reports whether a client must authenticate to use m.
func (m Module) AuthRequired() bool {
	return len(m.AuthUsers) > 0
}

// PasswordFunc resolves the password to use for daemon authentication.
// DialAuth calls it at most once, and only if the server sends an
// "@RSYNCD: AUTHREQD" challenge; a module that turns out not to require
// auth never triggers a call at all.
type PasswordFunc func() (string, error)

// StaticPassword wraps an already-known password as a PasswordFunc.
func StaticPassword(password string) PasswordFunc {
	return func() (string, error) { return password, nil }
}

// md4Hash returns the base64-encoded MD4 digest of secret followed by
// challenge, matching real rsync's generate_hash(): secret then challenge,
// no seed byte.
func md4Hash(secret, challenge string) string {
	h := md4.New()
	_, _ = io.WriteString(h, secret)
	_, _ = io.WriteString(h, challenge)
	return base64.RawStdEncoding.EncodeToString(h.Sum(nil))
}

// generateChallenge returns a fresh, random, base64-encoded challenge.
func generateChallenge() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generating challenge: %w", err)
	}
	return base64.RawStdEncoding.EncodeToString(buf), nil
}

// readSecretsFile parses a "name:secret" per-line secrets file.
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

// ServeAuth runs the server side of module authentication for m, which
// ServeGreeting has already selected. Password comparison is constant-time
// and the failure reason is never revealed on the wire, matching real
// rsync's single generic auth-failure message.
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

// DialAuth runs the client side of authentication: it reads lines until
// "@RSYNCD: OK", answering any "@RSYNCD: AUTHREQD <challenge>" with
// "<user> <md4Hash(password, challenge)>". The password itself is never
// sent on the wire. An empty user is sent as "nobody".
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
