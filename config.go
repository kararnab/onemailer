package onemailer

import (
	"crypto/x509"
	"fmt"
	"net"
	"net/mail"
	"net/url"
	"os"
	"strconv"
	"strings"
)

// Providers ({PREFIX}MAIL_PROVIDER).
const (
	ProviderNone = "none"
	ProviderSMTP = "smtp"
)

// TLS modes ({PREFIX}SMTP_TLS).
const (
	TLSStartTLS = "starttls"
	TLSImplicit = "tls"
	TLSNone     = "none"
)

// Config is the mail configuration. Its String method never prints the
// password.
type Config struct {
	Provider  string
	PublicURL string // the web origin links point at; "" when unset
	From      *mail.Address
	ReplyTo   *mail.Address // optional ({PREFIX}MAIL_REPLY_TO)
	Host      string
	Port      int
	TLS       string
	Username  string
	// AllowPlainHost is the one non-loopback host that may receive plain
	// SMTP ({PREFIX}SMTP_ALLOW_PLAIN_HOST): a development catcher on a
	// container network, such as Mailpit at "mailpit".
	AllowPlainHost string

	// Prefix is the environment-variable prefix the configuration was
	// loaded with (for example "LIBRARYZ_"); errors name variables with it.
	Prefix string

	password string
	// rootCAs replaces the system roots (a test server's self-signed
	// certificate); see WithRootCAs.
	rootCAs *x509.CertPool
}

// Enabled reports whether mail is sent.
func (c Config) Enabled() bool { return c.Provider == ProviderSMTP }

// Addr is host:port.
func (c Config) Addr() string { return net.JoinHostPort(c.Host, strconv.Itoa(c.Port)) }

// WithRootCAs returns c trusting only pool for the server certificate. For
// tests against mailtest's TLS modes; production uses the system roots.
func (c Config) WithRootCAs(pool *x509.CertPool) Config {
	c.rootCAs = pool
	return c
}

// plainAllowed reports whether plain SMTP may go to the configured host.
func (c Config) plainAllowed() bool {
	return IsLoopback(c.Host) || (c.AllowPlainHost != "" && strings.EqualFold(c.Host, c.AllowPlainHost))
}

// String describes the configuration for logs, without the password.
func (c Config) String() string {
	if !c.Enabled() {
		return "mail off"
	}
	auth := "no login"
	if c.Username != "" {
		auth = "login " + c.Username
	}
	s := fmt.Sprintf("smtp %s (%s, %s), from %s", c.Addr(), c.TLS, auth, c.From)
	if c.ReplyTo != nil {
		s += ", replies to " + c.ReplyTo.String()
	}
	return s
}

// GoString keeps %#v from printing the password too.
func (c Config) GoString() string { return c.String() }

// LoadConfig reads the mail settings from the environment, each variable
// named with prefix (for example "LIBRARYZ_"):
//
//	{P}MAIL_PROVIDER          none (default) or smtp
//	{P}PUBLIC_URL             absolute http(s) origin links point at; required with smtp;
//	                          http only to localhost or when {P}ENV=development
//	{P}MAIL_FROM              sender, "App <accounts@example.org>"; required with smtp
//	{P}MAIL_REPLY_TO          optional reply address
//	{P}SMTP_HOST              required with smtp
//	{P}SMTP_PORT              default 587
//	{P}SMTP_TLS               starttls (default), tls (implicit, usually 465) or none
//	{P}SMTP_ALLOW_PLAIN_HOST  the one non-loopback host allowed with none (dev catcher)
//	{P}SMTP_USERNAME          login, when the server requires one
//	{P}SMTP_PASSWORD          or {P}SMTP_PASSWORD_FILE (preferred); at most one
//
// Every error names its variable; none contains the password.
func LoadConfig(getenv func(string) string, prefix string) (Config, error) {
	env := func(name string) string { return getenv(prefix + name) }
	name := func(n string) string { return prefix + n }
	development := env("ENV") == "development"
	cfg := Config{Provider: ProviderNone, Port: 587, TLS: TLSStartTLS, Prefix: prefix}

	switch p := env("MAIL_PROVIDER"); p {
	case "", ProviderNone:
	case ProviderSMTP:
		cfg.Provider = ProviderSMTP
	default:
		return Config{}, fmt.Errorf("%s must be none or smtp, got %q", name("MAIL_PROVIDER"), p)
	}

	if s := env("PUBLIC_URL"); s != "" {
		u, err := url.Parse(s)
		bad := fmt.Errorf("%s must be an absolute http(s) URL with no query or fragment, got %q", name("PUBLIC_URL"), s)
		switch {
		case err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" ||
			strings.ContainsAny(s, "?#"):
			return Config{}, bad
		case u.Scheme == "https":
		case u.Scheme == "http" && (development || IsLoopback(u.Hostname())):
		case u.Scheme == "http":
			return Config{}, fmt.Errorf("%s must use https unless its host is localhost or %s=development, got %q",
				name("PUBLIC_URL"), name("ENV"), s)
		default:
			return Config{}, bad
		}
		cfg.PublicURL = strings.TrimRight(s, "/")
	}

	if !cfg.Enabled() {
		return cfg, nil
	}

	if cfg.PublicURL == "" {
		return Config{}, fmt.Errorf("%s is required when %s=smtp (the address links point at)", name("PUBLIC_URL"), name("MAIL_PROVIDER"))
	}
	from := env("MAIL_FROM")
	if from == "" {
		return Config{}, fmt.Errorf("%s is required when %s=smtp (for example: App <accounts@example.org>)", name("MAIL_FROM"), name("MAIL_PROVIDER"))
	}
	addr, err := mail.ParseAddress(from)
	if err != nil || strings.ContainsAny(from, "\r\n") || !plausibleAddress(addr.Address) {
		return Config{}, fmt.Errorf("%s must be an address such as App <accounts@example.org>, got %q", name("MAIL_FROM"), from)
	}
	cfg.From = addr
	if s := env("MAIL_REPLY_TO"); s != "" {
		rt, err := mail.ParseAddress(s)
		if err != nil || strings.ContainsAny(s, "\r\n") || !plausibleAddress(rt.Address) {
			return Config{}, fmt.Errorf("%s must be an address such as App support <support@example.org>, got %q", name("MAIL_REPLY_TO"), s)
		}
		cfg.ReplyTo = rt
	}

	cfg.Host = env("SMTP_HOST")
	if cfg.Host == "" {
		return Config{}, fmt.Errorf("%s is required when %s=smtp", name("SMTP_HOST"), name("MAIL_PROVIDER"))
	}
	if strings.ContainsAny(cfg.Host, " /:\r\n") && net.ParseIP(cfg.Host) == nil {
		return Config{}, fmt.Errorf("%s must be a host name or IP address, got %q", name("SMTP_HOST"), cfg.Host)
	}
	if s := env("SMTP_PORT"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 1 || n > 65535 {
			return Config{}, fmt.Errorf("%s must be a port from 1 to 65535, got %q", name("SMTP_PORT"), s)
		}
		cfg.Port = n
	}
	cfg.AllowPlainHost = env("SMTP_ALLOW_PLAIN_HOST")
	if strings.ContainsAny(cfg.AllowPlainHost, " /:\r\n") {
		return Config{}, fmt.Errorf("%s must be a host name, got %q", name("SMTP_ALLOW_PLAIN_HOST"), cfg.AllowPlainHost)
	}
	switch t := env("SMTP_TLS"); t {
	case "", TLSStartTLS:
	case TLSImplicit:
		cfg.TLS = TLSImplicit
	case TLSNone:
		if !cfg.plainAllowed() {
			return Config{}, fmt.Errorf("%s=none is allowed only to a loopback host (localhost, 127.0.0.1, ::1) or the host named by %s, not %q",
				name("SMTP_TLS"), name("SMTP_ALLOW_PLAIN_HOST"), cfg.Host)
		}
		cfg.TLS = TLSNone
	default:
		return Config{}, fmt.Errorf("%s must be starttls, tls or none, got %q", name("SMTP_TLS"), t)
	}

	cfg.Username = env("SMTP_USERNAME")
	pw, pwFile := env("SMTP_PASSWORD"), env("SMTP_PASSWORD_FILE")
	switch {
	case pw != "" && pwFile != "":
		return Config{}, fmt.Errorf("set at most one of %s and %s", name("SMTP_PASSWORD"), name("SMTP_PASSWORD_FILE"))
	case pwFile != "":
		raw, err := os.ReadFile(pwFile)
		if err != nil {
			return Config{}, fmt.Errorf("%s: cannot read %q", name("SMTP_PASSWORD_FILE"), pwFile)
		}
		cfg.password = strings.TrimRight(string(raw), "\r\n")
		if cfg.password == "" {
			return Config{}, fmt.Errorf("%s: %q is empty", name("SMTP_PASSWORD_FILE"), pwFile)
		}
	default:
		cfg.password = pw
	}
	if cfg.password != "" && cfg.Username == "" {
		return Config{}, fmt.Errorf("%s is required with an SMTP password", name("SMTP_USERNAME"))
	}
	return cfg, nil
}

// IsLoopback reports whether host is localhost or a loopback IP address.
func IsLoopback(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// plausibleAddress: one @, something before it, a dotted domain, no spaces.
func plausibleAddress(a string) bool {
	at := strings.IndexByte(a, '@')
	if at < 1 || at != strings.LastIndexByte(a, '@') {
		return false
	}
	labels := strings.Split(a[at+1:], ".")
	if len(labels) < 2 {
		return false
	}
	for _, l := range labels {
		if l == "" {
			return false
		}
	}
	return !strings.ContainsAny(a, " \t")
}
