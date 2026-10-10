package onemailer

import (
	"context"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net/mail"
	"strings"
	"testing"
	"time"

	"github.com/kararnab/onemailer/mailtest"
)

// configFor points a configuration at a test server.
func configFor(t *testing.T, srv *mailtest.Server, tlsMode string, over ...string) Config {
	t.Helper()
	env := smtpEnv(append([]string{
		"APP_SMTP_HOST", srv.Host, "APP_SMTP_PORT", srv.PortString(), "APP_SMTP_TLS", tlsMode,
	}, over...)...)
	cfg, err := LoadConfig(envOf(env), "APP_")
	if err != nil {
		t.Fatal(err)
	}
	cfg.rootCAs = srv.Roots
	return cfg
}

func send(t *testing.T, cfg Config, m Message) error {
	t.Helper()
	s, err := NewSMTPSender(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return s.Send(ctx, m)
}

var sample = Message{Kind: "test", To: "Ada <ada@example.com>", Subject: "Hello — test", Text: "Line one\nLine two é", HTML: "<p>Hi</p>"}

// AC-2: delivery through each TLS mode.
func TestSend_Modes(t *testing.T) {
	cases := []struct {
		name, srvMode, cfgTLS string
		wantTLS               bool
	}{
		{"starttls", mailtest.StartTLS, TLSStartTLS, true},
		{"implicit tls", mailtest.Implicit, TLSImplicit, true},
		{"plain to loopback", mailtest.Plain, TLSNone, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv := mailtest.Start(t, mailtest.Options{Mode: c.srvMode, Username: "u", Password: "p"})
			cfg := configFor(t, srv, c.cfgTLS, "APP_SMTP_USERNAME", "u", "APP_SMTP_PASSWORD", "p")
			if err := send(t, cfg, sample); err != nil {
				t.Fatal(err)
			}
			got := srv.Messages()
			if len(got) != 1 {
				t.Fatalf("messages: %d", len(got))
			}
			r := got[0]
			if r.From != "no-reply@codeatlas.example" || len(r.To) != 1 || r.To[0] != "ada@example.com" || r.TLS != c.wantTLS || r.AuthUser != "u" {
				t.Fatalf("envelope: %+v", r)
			}
			checkBody(t, r.Data)
		})
	}
}

// checkBody parses the message and checks its headers and both parts.
func checkBody(t *testing.T, data string) {
	t.Helper()
	msg, err := mail.ReadMessage(strings.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	subject, _ := new(mime.WordDecoder).DecodeHeader(msg.Header.Get("Subject"))
	if subject != "Hello — test" || msg.Header.Get("From") != `"App" <no-reply@codeatlas.example>` ||
		!strings.HasSuffix(msg.Header.Get("Message-ID"), "@codeatlas.example>") || msg.Header.Get("Date") == "" {
		t.Fatalf("headers: %v (subject %q)", msg.Header, subject)
	}
	mediaType, params, err := mime.ParseMediaType(msg.Header.Get("Content-Type"))
	if err != nil || mediaType != "multipart/alternative" {
		t.Fatalf("content type %q: %v", mediaType, err)
	}
	mr := multipart.NewReader(msg.Body, params["boundary"])
	var parts []string
	for {
		p, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		// multipart decodes quoted-printable; the server's dot reader turned
		// CRLF into LF.
		body, _ := io.ReadAll(p)
		parts = append(parts, p.Header.Get("Content-Type")+"|"+string(body))
	}
	if len(parts) != 2 || parts[0] != "text/plain; charset=UTF-8|Line one\nLine two é" || parts[1] != "text/html; charset=UTF-8|<p>Hi</p>" {
		t.Fatalf("parts: %q", parts)
	}
}

// AC-2: refusals.
func TestSend_Refusals(t *testing.T) {
	t.Run("reply-to header", func(t *testing.T) {
		srv := mailtest.Start(t, mailtest.Options{Mode: mailtest.Plain})
		if err := send(t, configFor(t, srv, TLSNone, "APP_MAIL_REPLY_TO", "App support <support@codeatlas.example>"), sample); err != nil {
			t.Fatal(err)
		}
		msg, err := mail.ReadMessage(strings.NewReader(srv.Messages()[0].Data))
		if err != nil || msg.Header.Get("Reply-To") != `"App support" <support@codeatlas.example>` {
			t.Fatalf("%v %v", msg.Header, err)
		}
	})
	t.Run("starttls not offered", func(t *testing.T) {
		srv := mailtest.Start(t, mailtest.Options{Mode: mailtest.Plain})
		err := send(t, configFor(t, srv, TLSNone), sample)
		if err != nil {
			t.Fatalf("plain control: %v", err)
		}
		cfg := configFor(t, srv, TLSNone)
		cfg.TLS = TLSStartTLS
		if err := send(t, cfg, sample); err == nil || !strings.Contains(err.Error(), "STARTTLS") {
			t.Fatalf("want a STARTTLS refusal, got %v", err)
		}
	})
	t.Run("certificate for another host", func(t *testing.T) {
		srv := mailtest.Start(t, mailtest.Options{Mode: mailtest.StartTLS, CertHost: "other.example"})
		if err := send(t, configFor(t, srv, TLSStartTLS), sample); err == nil || !strings.Contains(err.Error(), "certificate") {
			t.Fatalf("want a certificate error, got %v", err)
		}
		if len(srv.Messages()) != 0 {
			t.Fatal("delivered despite the certificate")
		}
	})
	t.Run("untrusted certificate", func(t *testing.T) {
		srv := mailtest.Start(t, mailtest.Options{Mode: mailtest.Implicit})
		cfg := configFor(t, srv, TLSImplicit)
		cfg.rootCAs = nil // system roots do not trust the test CA
		if err := send(t, cfg, sample); err == nil {
			t.Fatal("want a certificate error")
		}
	})
	t.Run("plain to a remote host", func(t *testing.T) {
		srv := mailtest.Start(t, mailtest.Options{Mode: mailtest.Plain})
		cfg := configFor(t, srv, TLSNone)
		cfg.Host = "smtp.example.com"
		if err := send(t, cfg, sample); err == nil || !strings.Contains(err.Error(), "loopback") {
			t.Fatalf("want a loopback refusal, got %v", err)
		}
	})
	t.Run("wrong password", func(t *testing.T) {
		srv := mailtest.Start(t, mailtest.Options{Mode: mailtest.StartTLS, Username: "u", Password: "p"})
		err := send(t, configFor(t, srv, TLSStartTLS, "APP_SMTP_USERNAME", "u", "APP_SMTP_PASSWORD", "bad"), sample)
		if SMTPCode(err) != 535 {
			t.Fatalf("want 535, got %v", err)
		}
	})
	t.Run("rejected recipient", func(t *testing.T) {
		srv := mailtest.Start(t, mailtest.Options{Mode: mailtest.Plain, RejectRcpt: true})
		if err := send(t, configFor(t, srv, TLSNone), sample); SMTPCode(err) != 550 {
			t.Fatalf("want 550, got %v", err)
		}
	})
	t.Run("header injection", func(t *testing.T) {
		srv := mailtest.Start(t, mailtest.Options{Mode: mailtest.Plain})
		cfg := configFor(t, srv, TLSNone)
		for _, m := range []Message{
			{To: "ada@example.com\r\nBcc: eve@example.com", Subject: "s", Text: "t"},
			{To: "ada@example.com", Subject: "s\nBcc: eve@example.com", Text: "t"},
		} {
			if err := send(t, cfg, m); !errors.Is(err, ErrHeaderInjection) {
				t.Fatalf("want ErrHeaderInjection, got %v", err)
			}
		}
		if len(srv.Messages()) != 0 {
			t.Fatal("delivered an injected message")
		}
	})
	t.Run("connection refused", func(t *testing.T) {
		srv := mailtest.Start(t, mailtest.Options{Mode: mailtest.Plain})
		cfg := configFor(t, srv, TLSNone)
		cfg.Port = 1 // nothing listens there
		if err := send(t, cfg, sample); err == nil {
			t.Fatal("want a connection error")
		}
	})
}

func TestNewSMTPSender_RefusesNone(t *testing.T) {
	if _, err := NewSMTPSender(Config{Provider: ProviderNone}); err == nil {
		t.Fatal("want an error for provider none")
	}
}

// A message without HTML is a single text/plain part.
func TestRender_TextOnly(t *testing.T) {
	from, _ := mail.ParseAddress("no-reply@codeatlas.example")
	to, _ := mail.ParseAddress("ada@example.com")
	data, err := render(from, nil, to, Message{To: "ada@example.com", Subject: "s", Text: "t"}, time.Unix(0, 0))
	if err != nil {
		t.Fatal(err)
	}
	msg, err := mail.ReadMessage(strings.NewReader(string(data)))
	if err != nil || msg.Header.Get("Content-Type") != "text/plain; charset=UTF-8" {
		t.Fatalf("%v %v", msg.Header, err)
	}
	if strings.Contains(string(data), "\n") && strings.Count(string(data), "\n") != strings.Count(string(data), "\r\n") {
		t.Fatal("a bare LF in the message")
	}
}
