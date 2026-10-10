package onemailer

import (
	"strings"
	"testing"
	"time"
)

func TestLoadConfigNamesVariablesWithThePrefix(t *testing.T) {
	_, err := LoadConfig(envOf(map[string]string{"LIBRARYZ_MAIL_PROVIDER": "carrier-pigeon"}), "LIBRARYZ_")
	if err == nil || !strings.Contains(err.Error(), "LIBRARYZ_MAIL_PROVIDER") {
		t.Fatalf("want an error naming LIBRARYZ_MAIL_PROVIDER, got %v", err)
	}
	// Another app's variables are ignored.
	cfg, err := LoadConfig(envOf(map[string]string{"OTHER_MAIL_PROVIDER": "smtp"}), "LIBRARYZ_")
	if err != nil || cfg.Enabled() {
		t.Fatalf("other prefix leaked in: %v %v", cfg, err)
	}
}

func smtpEnvWith(kv ...string) map[string]string {
	m := map[string]string{
		"APP_MAIL_PROVIDER": "smtp",
		"APP_PUBLIC_URL":    "https://example.org",
		"APP_MAIL_FROM":     "App <accounts@example.org>",
		"APP_SMTP_HOST":     "smtp.example.org",
	}
	for i := 0; i+1 < len(kv); i += 2 {
		m[kv[i]] = kv[i+1]
	}
	return m
}

func TestPlainSMTPOnlyToLoopbackOrTheNamedHost(t *testing.T) {
	cases := []struct {
		name string
		env  map[string]string
		ok   bool
	}{
		{"remote host refused", smtpEnvWith("APP_SMTP_TLS", "none"), false},
		{"loopback allowed", smtpEnvWith("APP_SMTP_TLS", "none", "APP_SMTP_HOST", "127.0.0.1"), true},
		{"named catcher allowed", smtpEnvWith("APP_SMTP_TLS", "none", "APP_SMTP_HOST", "mailpit", "APP_SMTP_ALLOW_PLAIN_HOST", "mailpit"), true},
		{"other host than the named one refused", smtpEnvWith("APP_SMTP_TLS", "none", "APP_SMTP_HOST", "smtp.example.org", "APP_SMTP_ALLOW_PLAIN_HOST", "mailpit"), false},
		{"bad allow value", smtpEnvWith("APP_SMTP_ALLOW_PLAIN_HOST", "mail pit"), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg, err := LoadConfig(envOf(c.env), "APP_")
			if (err == nil) != c.ok {
				t.Fatalf("ok=%v want %v (err %v)", err == nil, c.ok, err)
			}
			if c.ok && (cfg.TLS != TLSNone || !cfg.plainAllowed()) {
				t.Fatalf("plain not allowed: %+v", cfg)
			}
		})
	}
}

func TestPublicURLHTTPOnlyForLoopbackOrDevelopment(t *testing.T) {
	cases := []struct {
		env map[string]string
		ok  bool
	}{
		{smtpEnvWith("APP_PUBLIC_URL", "http://localhost:8081"), true},
		{smtpEnvWith("APP_PUBLIC_URL", "http://127.0.0.1:8081/"), true},
		{smtpEnvWith("APP_PUBLIC_URL", "http://library.example.org"), false},
		{smtpEnvWith("APP_PUBLIC_URL", "http://library.example.org", "APP_ENV", "development"), true},
	}
	for _, c := range cases {
		cfg, err := LoadConfig(envOf(c.env), "APP_")
		if (err == nil) != c.ok {
			t.Fatalf("%s: ok=%v want %v (err %v)", c.env["APP_PUBLIC_URL"], err == nil, c.ok, err)
		}
		if c.ok && strings.HasSuffix(cfg.PublicURL, "/") {
			t.Fatalf("trailing slash kept: %q", cfg.PublicURL)
		}
	}
}

func TestLifetime(t *testing.T) {
	for d, want := range map[time.Duration]string{
		time.Hour:        "1 hour",
		48 * time.Hour:   "48 hours",
		30 * time.Minute: "30 minutes",
		90 * time.Minute: "90 minutes",
		time.Second:      "1 minute",
	} {
		if got := Lifetime(d); got != want {
			t.Errorf("Lifetime(%v) = %q, want %q", d, got, want)
		}
	}
}
