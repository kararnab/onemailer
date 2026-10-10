package onemailer

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net/mail"
	"net/smtp"
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
		if _, err := NewSMTPSender(cfg); err == nil || !strings.Contains(err.Error(), "loopback") {
			t.Fatalf("want a loopback refusal, got %v", err)
		}
		// Send refuses too, should a sender be built around the check.
		s := &SMTPSender{cfg: cfg, now: time.Now}
		if err := s.Send(context.Background(), sample); !IsPermanent(err) || !strings.Contains(err.Error(), "loopback") {
			t.Fatalf("want a permanent loopback refusal, got %v", err)
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

// A Config built in code gets LoadConfig's defaults and refusals.
func TestNewSMTPSender_HandBuiltConfig(t *testing.T) {
	from, _ := mail.ParseAddress("App <accounts@example.org>")
	base := Config{Provider: ProviderSMTP, From: from, Host: "smtp.example.org"}
	s, err := NewSMTPSender(base)
	if err != nil || s.cfg.TLS != TLSStartTLS || s.cfg.Port != 587 {
		t.Fatalf("defaults: %+v %v", s, err)
	}
	for name, cfg := range map[string]Config{
		"no from":           {Provider: ProviderSMTP, Host: "smtp.example.org"},
		"no host":           {Provider: ProviderSMTP, From: from},
		"unknown tls":       func() Config { c := base; c.TLS = "ssl"; return c }(),
		"plain to remote":   func() Config { c := base; c.TLS = TLSNone; return c }(),
		"password no login": base.WithPassword("p"),
	} {
		if _, err := NewSMTPSender(cfg); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
	allowed := base
	allowed.TLS, allowed.AllowPlainHost = TLSNone, "SMTP.example.org"
	if _, err := NewSMTPSender(allowed); err != nil {
		t.Fatalf("allowed plain host: %v", err)
	}
}

// WithPassword authenticates a Config that never saw the environment.
func TestSend_HandBuiltConfigWithPassword(t *testing.T) {
	srv := mailtest.Start(t, mailtest.Options{Mode: mailtest.StartTLS, Username: "u", Password: "p"})
	from, _ := mail.ParseAddress("App <no-reply@codeatlas.example>")
	cfg := Config{Provider: ProviderSMTP, From: from, Host: "localhost", Port: srv.Port, Username: "u"}.
		WithPassword("p").WithRootCAs(srv.Roots)
	if err := send(t, cfg, sample); err != nil {
		t.Fatal(err)
	}
	if got := srv.Messages(); len(got) != 1 || got[0].AuthUser != "u" || !got[0].TLS {
		t.Fatalf("%+v", got)
	}
}

// A server offering only AUTH LOGIN (Microsoft 365) is logged in to.
func TestSend_AuthLogin(t *testing.T) {
	for _, mode := range []struct{ srv, cfg string }{{mailtest.StartTLS, TLSStartTLS}, {mailtest.Implicit, TLSImplicit}} {
		srv := mailtest.Start(t, mailtest.Options{Mode: mode.srv, Username: "u", Password: "p", AuthMechs: []string{"LOGIN"}})
		cfg := configFor(t, srv, mode.cfg, "APP_SMTP_USERNAME", "u", "APP_SMTP_PASSWORD", "p")
		if err := send(t, cfg, sample); err != nil {
			t.Fatalf("%s: %v", mode.cfg, err)
		}
		if got := srv.Messages(); len(got) != 1 || got[0].AuthUser != "u" {
			t.Fatalf("%s: %+v", mode.cfg, got)
		}
		bad := configFor(t, srv, mode.cfg, "APP_SMTP_USERNAME", "u", "APP_SMTP_PASSWORD", "bad")
		if err := send(t, bad, sample); SMTPCode(err) != 535 || !IsPermanent(err) {
			t.Fatalf("%s: want a permanent 535, got %v", mode.cfg, err)
		}
	}
}

// LOGIN, like PLAIN, never sends credentials in clear to a remote host.
func TestLoginAuth_RefusesCleartext(t *testing.T) {
	a := &loginAuth{username: "u", password: "p", host: "smtp.example.org"}
	if _, _, err := a.Start(&smtp.ServerInfo{Name: "smtp.example.org", TLS: false}); err == nil {
		t.Fatal("want a refusal without TLS")
	}
	if _, _, err := a.Start(&smtp.ServerInfo{Name: "smtp.example.org", TLS: true}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := a.Start(&smtp.ServerInfo{Name: "evil.example", TLS: true}); err == nil {
		t.Fatal("want a refusal for another host")
	}
}

func TestSend_PermanentOrNot(t *testing.T) {
	srv := mailtest.Start(t, mailtest.Options{Mode: mailtest.Plain, RejectRcpt: true})
	if err := send(t, configFor(t, srv, TLSNone), sample); !IsPermanent(err) {
		t.Fatalf("550: want permanent, got %v", err)
	}
	err := send(t, configFor(t, srv, TLSNone), Message{To: "ada@example.com", Subject: "s\nBcc: x@example.com", Text: "t"})
	if !IsPermanent(err) || !errors.Is(err, ErrHeaderInjection) {
		t.Fatalf("injection: want permanent ErrHeaderInjection, got %v", err)
	}
	flaky := mailtest.Start(t, mailtest.Options{Mode: mailtest.Plain, FailData: 1})
	if err := send(t, configFor(t, flaky, TLSNone), sample); SMTPCode(err) != 451 || IsPermanent(err) {
		t.Fatalf("451: want temporary, got %v", err)
	}
	if IsPermanent(nil) || Permanent(nil) != nil {
		t.Fatal("nil is not a failure")
	}
}

// Extra headers and attachments: multipart/mixed around the alternatives.
func TestSend_HeadersAndAttachments(t *testing.T) {
	srv := mailtest.Start(t, mailtest.Options{Mode: mailtest.Plain})
	pdf := bytes.Repeat([]byte("%PDF-1.7 \x00\xff"), 40) // longer than one base64 line
	m := sample
	m.Headers = map[string]string{
		"List-Unsubscribe": "<https://codeatlas.example/unsubscribe/abc>",
		"X-Entity-Ref-ID":  "reset-42",
		"X-Note":           "Grüße",
	}
	m.Attachments = []Attachment{
		{Filename: "receipt.pdf", Data: pdf},
		{Filename: "Einladung über.ics", ContentType: "text/calendar; method=REQUEST", Data: []byte("BEGIN:VCALENDAR\r\nEND:VCALENDAR\r\n")},
	}
	if err := send(t, configFor(t, srv, TLSNone), m); err != nil {
		t.Fatal(err)
	}
	msg, err := mail.ReadMessage(strings.NewReader(srv.Messages()[0].Data))
	if err != nil {
		t.Fatal(err)
	}
	note, _ := new(mime.WordDecoder).DecodeHeader(msg.Header.Get("X-Note"))
	if msg.Header.Get("List-Unsubscribe") != m.Headers["List-Unsubscribe"] || msg.Header.Get("X-Entity-Ref-ID") != "reset-42" || note != "Grüße" {
		t.Fatalf("headers: %v", msg.Header)
	}
	mediaType, params, _ := mime.ParseMediaType(msg.Header.Get("Content-Type"))
	if mediaType != "multipart/mixed" {
		t.Fatalf("content type %q", mediaType)
	}
	mr := multipart.NewReader(msg.Body, params["boundary"])
	body, err := mr.NextPart()
	if err != nil {
		t.Fatal(err)
	}
	if typ, _, _ := mime.ParseMediaType(body.Header.Get("Content-Type")); typ != "multipart/alternative" {
		t.Fatalf("first part %q", typ)
	}
	for i, want := range []struct {
		typ, name string
		data      []byte
	}{{"application/pdf", "receipt.pdf", pdf}, {"text/calendar", "Einladung über.ics", m.Attachments[1].Data}} {
		p, err := mr.NextPart()
		if err != nil {
			t.Fatal(err)
		}
		typ, _, _ := mime.ParseMediaType(p.Header.Get("Content-Type"))
		raw, _ := io.ReadAll(base64.NewDecoder(base64.StdEncoding, p))
		if typ != want.typ || p.FileName() != want.name || !bytes.Equal(raw, want.data) {
			t.Fatalf("attachment %d: %q %q %d bytes", i, typ, p.FileName(), len(raw))
		}
	}
	if _, err := mr.NextPart(); err != io.EOF {
		t.Fatalf("want the end, got %v", err)
	}
}

func TestSend_RefusesBadHeadersAndAttachments(t *testing.T) {
	srv := mailtest.Start(t, mailtest.Options{Mode: mailtest.Plain})
	cfg := configFor(t, srv, TLSNone)
	for name, mod := range map[string]func(*Message){
		"value line break":   func(m *Message) { m.Headers = map[string]string{"X-A": "a\r\nBcc: eve@example.com"} },
		"name line break":    func(m *Message) { m.Headers = map[string]string{"X-A\r\nBcc": "eve@example.com"} },
		"name with colon":    func(m *Message) { m.Headers = map[string]string{"X-A: b": "c"} },
		"bcc":                func(m *Message) { m.Headers = map[string]string{"bcc": "eve@example.com"} },
		"from":               func(m *Message) { m.Headers = map[string]string{"From": "eve@example.com"} },
		"content header":     func(m *Message) { m.Headers = map[string]string{"Content-Type": "text/plain"} },
		"attachment no name": func(m *Message) { m.Attachments = []Attachment{{Data: []byte("x")}} },
		"attachment newline": func(m *Message) { m.Attachments = []Attachment{{Filename: "a\r\n.txt"}} },
		"attachment type":    func(m *Message) { m.Attachments = []Attachment{{Filename: "a.txt", ContentType: "not a type"}} },
	} {
		m := sample
		mod(&m)
		if err := send(t, cfg, m); err == nil || !IsPermanent(err) {
			t.Errorf("%s: want a permanent refusal, got %v", name, err)
		}
	}
	if len(srv.Messages()) != 0 {
		t.Fatal("delivered a refused message")
	}
}
