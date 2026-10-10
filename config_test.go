package onemailer

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func envOf(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

// smtpEnv is a valid smtp configuration; tests override single variables.
func smtpEnv(over ...string) map[string]string {
	m := map[string]string{
		"APP_MAIL_PROVIDER": "smtp",
		"APP_PUBLIC_URL":    "https://codeatlas.example",
		"APP_MAIL_FROM":     "App <no-reply@codeatlas.example>",
		"APP_SMTP_HOST":     "smtp.example.com",
	}
	for i := 0; i+1 < len(over); i += 2 {
		if over[i+1] == "<unset>" {
			delete(m, over[i])
		} else {
			m[over[i]] = over[i+1]
		}
	}
	return m
}

// §15.7: defaults, and none needs nothing else.
func TestLoadConfig_Defaults(t *testing.T) {
	cfg, err := LoadConfig(envOf(nil), "APP_")
	if err != nil || cfg.Enabled() || cfg.Provider != ProviderNone || cfg.String() != "mail off" || cfg.ReplyTo != nil {
		t.Fatalf("default: %v, %v", cfg, err)
	}
	cfg, err = LoadConfig(envOf(smtpEnv()), "APP_")
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Enabled() || cfg.Port != 587 || cfg.TLS != TLSStartTLS || cfg.From.Address != "no-reply@codeatlas.example" ||
		cfg.From.Name != "App" || cfg.PublicURL != "https://codeatlas.example" || cfg.Addr() != "smtp.example.com:587" {
		t.Fatalf("smtp defaults: %#v", cfg)
	}
}

// AC-1: every rule of §15.7, each error naming its variable.
func TestLoadConfig_Rules(t *testing.T) {
	cases := []struct {
		name    string
		env     map[string]string
		wantVar string
	}{
		{"unknown provider", smtpEnv("APP_MAIL_PROVIDER", "ses"), "APP_MAIL_PROVIDER"},
		{"public url missing", smtpEnv("APP_PUBLIC_URL", "<unset>"), "APP_PUBLIC_URL"},
		{"public url relative", smtpEnv("APP_PUBLIC_URL", "/reset"), "APP_PUBLIC_URL"},
		{"public url with query", smtpEnv("APP_PUBLIC_URL", "https://x.example/?a=1"), "APP_PUBLIC_URL"},
		{"public url with fragment", smtpEnv("APP_PUBLIC_URL", "https://x.example/#a"), "APP_PUBLIC_URL"},
		{"public url ftp", smtpEnv("APP_PUBLIC_URL", "ftp://x.example"), "APP_PUBLIC_URL"},
		{"public url http outside development", smtpEnv("APP_PUBLIC_URL", "http://x.example"), "https"},
		{"from missing", smtpEnv("APP_MAIL_FROM", "<unset>"), "APP_MAIL_FROM"},
		{"from malformed", smtpEnv("APP_MAIL_FROM", "App"), "APP_MAIL_FROM"},
		{"from without dotted domain", smtpEnv("APP_MAIL_FROM", "a@localhost"), "APP_MAIL_FROM"},
		{"host missing", smtpEnv("APP_SMTP_HOST", "<unset>"), "APP_SMTP_HOST"},
		{"host with port", smtpEnv("APP_SMTP_HOST", "smtp.example.com:587"), "APP_SMTP_HOST"},
		{"port zero", smtpEnv("APP_SMTP_PORT", "0"), "APP_SMTP_PORT"},
		{"port too big", smtpEnv("APP_SMTP_PORT", "70000"), "APP_SMTP_PORT"},
		{"port text", smtpEnv("APP_SMTP_PORT", "smtp"), "APP_SMTP_PORT"},
		{"tls unknown", smtpEnv("APP_SMTP_TLS", "ssl"), "APP_SMTP_TLS"},
		{"tls none to a remote host", smtpEnv("APP_SMTP_TLS", "none"), "APP_SMTP_TLS=none"},
		{"both passwords", smtpEnv("APP_SMTP_USERNAME", "u", "APP_SMTP_PASSWORD", "hunter2-secret", "APP_SMTP_PASSWORD_FILE", "/x"), "APP_SMTP_PASSWORD_FILE"},
		{"password file missing", smtpEnv("APP_SMTP_USERNAME", "u", "APP_SMTP_PASSWORD_FILE", "/does/not/exist"), "APP_SMTP_PASSWORD_FILE"},
		{"password without username", smtpEnv("APP_SMTP_PASSWORD", "hunter2-secret"), "APP_SMTP_USERNAME"},
		{"reply-to malformed", smtpEnv("APP_MAIL_REPLY_TO", "support"), "APP_MAIL_REPLY_TO"},
		{"reply-to without dotted domain", smtpEnv("APP_MAIL_REPLY_TO", "s@localhost"), "APP_MAIL_REPLY_TO"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := LoadConfig(envOf(c.env), "APP_")
			if err == nil {
				t.Fatalf("want an error naming %s", c.wantVar)
			}
			if !strings.Contains(err.Error(), c.wantVar) {
				t.Fatalf("error %q does not name %s", err, c.wantVar)
			}
			if strings.Contains(err.Error(), "hunter2-secret") {
				t.Fatalf("error %q prints the password", err)
			}
		})
	}
}

func TestLoadConfig_Accepted(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "pw")
	if err := os.WriteFile(file, []byte("from-file-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name  string
		env   map[string]string
		check func(Config) bool
	}{
		{"http in development", smtpEnv("APP_ENV", "development", "APP_PUBLIC_URL", "http://localhost:5173/"),
			func(c Config) bool { return c.PublicURL == "http://localhost:5173" }},
		{"public url with a path", smtpEnv("APP_PUBLIC_URL", "https://x.example/app/"),
			func(c Config) bool { return c.PublicURL == "https://x.example/app" }},
		{"public url without mail", map[string]string{"APP_PUBLIC_URL": "https://x.example"},
			func(c Config) bool { return !c.Enabled() && c.PublicURL == "https://x.example" }},
		{"implicit tls", smtpEnv("APP_SMTP_TLS", "tls", "APP_SMTP_PORT", "465"),
			func(c Config) bool { return c.TLS == TLSImplicit && c.Port == 465 }},
		{"none to 127.0.0.1", smtpEnv("APP_SMTP_TLS", "none", "APP_SMTP_HOST", "127.0.0.1", "APP_SMTP_PORT", "1025"),
			func(c Config) bool { return c.TLS == TLSNone }},
		{"none to localhost", smtpEnv("APP_SMTP_TLS", "none", "APP_SMTP_HOST", "localhost"),
			func(c Config) bool { return c.TLS == TLSNone }},
		{"none to ::1", smtpEnv("APP_SMTP_TLS", "none", "APP_SMTP_HOST", "::1"),
			func(c Config) bool { return c.TLS == TLSNone && c.Addr() == "[::1]:587" }},
		{"bare from address", smtpEnv("APP_MAIL_FROM", "no-reply@codeatlas.example"),
			func(c Config) bool { return c.From.Address == "no-reply@codeatlas.example" }},
		{"reply-to", smtpEnv("APP_MAIL_REPLY_TO", "App support <support@codeatlas.example>"),
			func(c Config) bool {
				return c.ReplyTo.Address == "support@codeatlas.example" && strings.Contains(c.String(), "replies to")
			}},
		{"password", smtpEnv("APP_SMTP_USERNAME", "u", "APP_SMTP_PASSWORD", "p"),
			func(c Config) bool { return c.Username == "u" && c.password == "p" }},
		{"password file", smtpEnv("APP_SMTP_USERNAME", "u", "APP_SMTP_PASSWORD_FILE", file),
			func(c Config) bool { return c.password == "from-file-secret" }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg, err := LoadConfig(envOf(c.env), "APP_")
			if err != nil || !c.check(cfg) {
				t.Fatalf("%#v, %v", cfg, err)
			}
		})
	}
}

// The password never appears when a configuration is printed.
func TestConfig_StringHidesPassword(t *testing.T) {
	cfg, err := LoadConfig(envOf(smtpEnv("APP_SMTP_USERNAME", "user", "APP_SMTP_PASSWORD", "hunter2-secret")), "APP_")
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{cfg.String(), fmt.Sprint(cfg), fmt.Sprintf("%v %+v %#v %s", cfg, cfg, cfg, cfg)} {
		if strings.Contains(s, "hunter2-secret") {
			t.Fatalf("printed the password: %s", s)
		}
	}
	if !strings.Contains(cfg.String(), "smtp.example.com:587") || !strings.Contains(cfg.String(), "login user") {
		t.Fatalf("String: %s", cfg)
	}
}

func TestLoadConfig_OptionalPublicURL(t *testing.T) {
	env := smtpEnv("APP_PUBLIC_URL", "<unset>")
	if _, err := LoadConfig(envOf(env), "APP_"); err == nil || !strings.Contains(err.Error(), "APP_PUBLIC_URL") {
		t.Fatalf("want APP_PUBLIC_URL required by default, got %v", err)
	}
	cfg, err := LoadConfig(envOf(env), "APP_", OptionalPublicURL())
	if err != nil || !cfg.Enabled() || cfg.PublicURL != "" {
		t.Fatalf("%v %v", cfg, err)
	}
	// Still validated when set.
	if _, err := LoadConfig(envOf(smtpEnv("APP_PUBLIC_URL", "ftp://x")), "APP_", OptionalPublicURL()); err == nil {
		t.Fatal("want an error for a bad APP_PUBLIC_URL")
	}
}
