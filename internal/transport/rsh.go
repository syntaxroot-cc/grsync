package transport

import "strings"

// sshAddressFamilyFlag returns "-4" or "-6" to insert into the
// remote-shell argv when ipv4/ipv6 was requested AND program is
// genuinely ssh - real rsync's own exact behavior, verified against
// upstream source (main.c's do_cmd(): "if (default_af_hint == AF_INET
// && strcmp(t, "ssh") == 0) args[argc++] = "-4";", where t is the
// resolved remote-shell command's basename). Real rsync's own -4/-6 is
// never forwarded for any other remote shell - its own man page says so
// explicitly: "For other remote shells you'll need to specify '--rsh
// SHELL -4' directly" - so this returns "" (nothing to insert) for
// anything but ssh, rather than guessing at another program's own
// address-family flag syntax.
//
// program is compared by basename, not the full path, matching real
// rsync's own strrchr(cmd, '/')-based check - and additionally strips a
// trailing ".exe", which upstream's Unix-only C code never needs to but
// grsync does, running natively on Windows too.
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
// or empty to use DefaultRSH. This is deliberately the *only*
// customization mechanism here - there's no separate --port or
// --identity flag, matching real rsync: upstream's --port only applies to
// daemon-mode (rsync://) connections, not the remote-shell transport, and
// its -i flag already means --itemize-changes, not "identity file" - so
// port/identity/ProxyJump/etc. are customized via -e (e.g.
// -e "ssh -p 2222 -i key.pem") or the user's own ~/.ssh/config, exactly as
// with real rsync. Inventing grsync-only flags for these would be a
// *worse* match for SC-1's parity goal, not a better one.
//
// ipv4/ipv6 (--ipv4/--ipv6, SC-14) are the one deliberate exception to
// that "customize everything via -e" philosophy, because real rsync
// itself makes it one: see sshAddressFamilyFlag's own doc comment for
// exactly when -4/-6 does and doesn't get inserted here, verified
// against upstream's own source rather than assumed.
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
// fields, honoring single- and double-quoted substrings so an argument
// containing spaces (e.g. -e `ssh -o "ProxyCommand=nc %h %p"`) survives as
// one field instead of being split apart - strings.Fields alone would
// silently mis-split exactly that case, which is common enough in real
// -e usage to be worth handling.
//
// This is not a full POSIX shell parser: no backslash escaping, no
// nested/mixed quotes, no variable expansion - just enough for the common
// case of one quoted argument. An unterminated quote is not treated as an
// error; whatever follows the opening quote to the end of the string
// becomes that field's content, which is a reasonable low-stakes fallback
// for malformed CLI input rather than something worth failing on.
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
