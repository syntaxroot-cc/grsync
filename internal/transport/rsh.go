package transport

import "strings"

// sshAddressFamilyFlag returns "-4" or "-6" to insert into the argv when
// ipv4/ipv6 was requested and the resolved program (matched by basename,
// ignoring a trailing ".exe") is ssh; it returns "" for any other remote
// shell, matching real rsync's own behavior.
func sshAddressFamilyFlag(program string, ipv4, ipv6 bool) string {
	if !ipv4 && !ipv6 {
		return ""
	}
	base := program
	if i := strings.LastIndexAny(base, `/\`); i >= 0 {
		base = base[i+1:]
	}
	base = strings.TrimSuffix(base, ".exe")
	if base != "ssh" {
		return ""
	}
	if ipv4 {
		return "-4"
	}
	return "-6"
}

// DefaultRSH is the remote-shell command used when no --rsh/-e override is
// given, matching real rsync's own default.
const DefaultRSH = "ssh"

// BuildRSHCommand builds the argv for invoking the remote shell to reach
// host (optionally as user@host) and run remoteArgs there, e.g.
// ["ssh", "user@host", "grsync", "--server"].
//
// rsh is the raw --rsh/-e override string (e.g. "ssh -p 2222 -i key.pem"),
// or empty to use DefaultRSH; it's the only customization mechanism -
// there's no separate --port or --identity flag, matching real rsync.
// ipv4/ipv6 additionally insert -4/-6 per sshAddressFamilyFlag.
func BuildRSHCommand(rsh, user, host string, remoteArgs []string, ipv4, ipv6 bool) []string {
	fields := splitRSHCommand(rsh)
	if len(fields) == 0 {
		fields = []string{DefaultRSH}
	}

	target := host
	if user != "" {
		target = user + "@" + host
	}

	cmd := make([]string, 0, len(fields)+2+len(remoteArgs))
	cmd = append(cmd, fields...)
	if af := sshAddressFamilyFlag(fields[0], ipv4, ipv6); af != "" {
		cmd = append(cmd, af)
	}
	cmd = append(cmd, target)
	cmd = append(cmd, remoteArgs...)
	return cmd
}

// splitRSHCommand splits an --rsh/-e command string into argv-style
// fields, honoring single- and double-quoted substrings so a quoted
// argument containing spaces survives as one field.
//
// This is not a full shell parser: no escaping, no nested quotes, and an
// unterminated quote just runs to the end of the string rather than erroring.
func splitRSHCommand(s string) []string {
	var fields []string
	var current strings.Builder
	var inQuote byte

	flush := func() {
		if current.Len() > 0 {
			fields = append(fields, current.String())
			current.Reset()
		}
	}

	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case inQuote != 0:
			if c == inQuote {
				inQuote = 0
			} else {
				current.WriteByte(c)
			}
		case c == '\'' || c == '"':
			inQuote = c
		case c == ' ' || c == '\t':
			flush()
		default:
			current.WriteByte(c)
		}
	}
	flush()
	return fields
}
