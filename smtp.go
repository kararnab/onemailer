package onemailer

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/smtp"
	"net/textproto"
	"slices"
	"strings"
	"time"
)

// Sender delivers one message.
type Sender interface {
	Send(ctx context.Context, m Message) error
}

// Timeouts for one delivery (one connection per message).
const (
	dialTimeout = 15 * time.Second
	sendTimeout = 30 * time.Second
)

// SMTPSender delivers over SMTP with the configured TLS mode. It verifies
// certificates against the host name and never skips verification.
type SMTPSender struct {
	cfg Config
	now func() time.Time
}

// NewSMTPSender returns a sender for an smtp configuration. A Config built
// by hand rather than by LoadConfig gets the same defaults (port 587,
// starttls) and the same refusals: no sender, no host, an unknown TLS mode,
// or plain SMTP to a host that is neither loopback nor AllowPlainHost.
func NewSMTPSender(cfg Config) (*SMTPSender, error) {
	if !cfg.Enabled() {
		return nil, fmt.Errorf("onemailer: %sMAIL_PROVIDER is none", cfg.Prefix)
	}
	if cfg.From == nil || cfg.Host == "" {
		return nil, errors.New("onemailer: the configuration needs From and Host")
	}
	if cfg.Port == 0 {
		cfg.Port = 587
	}
	switch cfg.TLS {
	case "":
		cfg.TLS = TLSStartTLS
	case TLSStartTLS, TLSImplicit:
	case TLSNone:
		if !cfg.plainAllowed() {
			return nil, errors.New("onemailer: plain SMTP is allowed only to a loopback host or the allowed plain host")
		}
	default:
		return nil, fmt.Errorf("onemailer: TLS must be starttls, tls or none, got %q", cfg.TLS)
	}
	if cfg.password != "" && cfg.Username == "" {
		return nil, errors.New("onemailer: a password needs a username")
	}
	return &SMTPSender{cfg: cfg, now: time.Now}, nil
}

// ErrMailOff is returned by the sender NewSender gives for provider none.
// Queue logs it as reason=mail_off, not as a failure.
var ErrMailOff = errors.New("onemailer: mail is off (MAIL_PROVIDER=none)")

// NewSender returns the sender for cfg: an SMTPSender when mail is on, and
// when it is off a sender that delivers nothing and answers ErrMailOff, so
// an application can always build its Queue the same way.
func NewSender(cfg Config) (Sender, error) {
	if !cfg.Enabled() {
		return offSender{}, nil
	}
	return NewSMTPSender(cfg)
}

type offSender struct{}

func (offSender) Send(context.Context, Message) error { return ErrMailOff }

// ErrPermanent matches (with errors.Is) a failure that sending again cannot
// fix: an invalid message, a configuration the server refuses, or an SMTP
// 5xx reply. Queue does not retry these.
var ErrPermanent = errors.New("onemailer: permanent failure")

type permanentError struct{ error }

func (e permanentError) Unwrap() error      { return e.error }
func (permanentError) Is(target error) bool { return target == ErrPermanent }

// Permanent marks err as not worth retrying, for a Sender of your own.
func Permanent(err error) error {
	if err == nil {
		return nil
	}
	return permanentError{err}
}

// IsPermanent reports whether retrying err is pointless: it was marked with
// Permanent, or it carries an SMTP 5xx reply (bad recipient, refused login).
func IsPermanent(err error) bool {
	if errors.Is(err, ErrPermanent) || errors.Is(err, ErrMailOff) {
		return true
	}
	code := SMTPCode(err)
	return code >= 500 && code < 600
}

// SMTPCode returns the SMTP reply code inside err, or 0.
func SMTPCode(err error) int {
	var tp *textproto.Error
	if errors.As(err, &tp) {
		return tp.Code
	}
	return 0
}

// Send connects, upgrades or starts TLS as configured, authenticates when a
// username is set, and delivers m.
func (s *SMTPSender) Send(ctx context.Context, m Message) error {
	to, err := m.validate()
	if err != nil {
		return Permanent(err)
	}
	data, err := render(s.cfg.From, s.cfg.ReplyTo, to, m, s.now())
	if err != nil {
		return err
	}
	if s.cfg.TLS == TLSNone && !s.cfg.plainAllowed() {
		return Permanent(errors.New("onemailer: plain SMTP is allowed only to a loopback host or the allowed plain host"))
	}

	deadline := time.Now().Add(sendTimeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	dialer := &net.Dialer{Timeout: dialTimeout, Deadline: deadline}
	tlsConfig := &tls.Config{ServerName: s.cfg.Host, RootCAs: s.cfg.rootCAs, MinVersion: tls.VersionTLS12}

	var conn net.Conn
	if s.cfg.TLS == TLSImplicit {
		conn, err = (&tls.Dialer{NetDialer: dialer, Config: tlsConfig}).DialContext(ctx, "tcp", s.cfg.Addr())
	} else {
		conn, err = dialer.DialContext(ctx, "tcp", s.cfg.Addr())
	}
	if err != nil {
		return fmt.Errorf("onemailer: connect to %s: %w", s.cfg.Addr(), err)
	}
	if err := conn.SetDeadline(deadline); err != nil {
		conn.Close()
		return err
	}
	// Abort a slow exchange when ctx ends.
	stop := context.AfterFunc(ctx, func() { conn.SetDeadline(time.Unix(1, 0)) })
	defer stop()

	c, err := smtp.NewClient(conn, s.cfg.Host)
	if err != nil {
		conn.Close()
		return fmt.Errorf("onemailer: greeting: %w", err)
	}
	defer c.Close()
	if err := c.Hello(domainOf(s.cfg.From.Address)); err != nil {
		return fmt.Errorf("onemailer: EHLO: %w", err)
	}
	if s.cfg.TLS == TLSStartTLS {
		if ok, _ := c.Extension("STARTTLS"); !ok {
			return Permanent(fmt.Errorf("onemailer: the server does not offer STARTTLS (%sSMTP_TLS=starttls)", s.cfg.Prefix))
		}
		if err := c.StartTLS(tlsConfig); err != nil {
			return fmt.Errorf("onemailer: STARTTLS: %w", err)
		}
	}
	if s.cfg.Username != "" {
		if err := c.Auth(s.auth(c)); err != nil {
			return fmt.Errorf("onemailer: AUTH: %w", err)
		}
	}
	if err := c.Mail(s.cfg.From.Address); err != nil {
		return fmt.Errorf("onemailer: MAIL FROM: %w", err)
	}
	if err := c.Rcpt(to.Address); err != nil {
		return fmt.Errorf("onemailer: RCPT TO: %w", err)
	}
	w, err := c.Data()
	if err != nil {
		return fmt.Errorf("onemailer: DATA: %w", err)
	}
	if _, err := w.Write(data); err != nil {
		return fmt.Errorf("onemailer: DATA: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("onemailer: DATA: %w", err)
	}
	// The message was accepted at the end of DATA; a failed QUIT does not
	// undo that.
	_ = c.Quit()
	return nil
}

// auth picks PLAIN, or LOGIN when the server offers LOGIN and not PLAIN
// (Microsoft 365 / Exchange Online, some hosting relays). Both refuse to
// send credentials without TLS unless the host is loopback: credentials
// never cross the wire in clear.
func (s *SMTPSender) auth(c *smtp.Client) smtp.Auth {
	_, offered := c.Extension("AUTH")
	mechs := strings.Fields(strings.ToUpper(offered))
	if !slices.Contains(mechs, "PLAIN") && slices.Contains(mechs, "LOGIN") {
		return &loginAuth{username: s.cfg.Username, password: s.cfg.password, host: s.cfg.Host}
	}
	return smtp.PlainAuth("", s.cfg.Username, s.cfg.password, s.cfg.Host)
}

// loginAuth is the AUTH LOGIN mechanism, which net/smtp lacks.
type loginAuth struct {
	username, password, host string
	step                     int
}

func (a *loginAuth) Start(server *smtp.ServerInfo) (string, []byte, error) {
	if !server.TLS && !IsLoopback(server.Name) {
		return "", nil, errors.New("unencrypted connection")
	}
	if server.Name != a.host {
		return "", nil, errors.New("wrong host name")
	}
	a.step = 0
	return "LOGIN", nil, nil
}

// Next answers the server's prompts: "Username:", then "Password:". Servers
// word them differently, so an unrecognised prompt is answered by its place.
func (a *loginAuth) Next(fromServer []byte, more bool) ([]byte, error) {
	if !more {
		return nil, nil
	}
	a.step++
	prompt := strings.ToLower(string(fromServer))
	switch {
	case strings.Contains(prompt, "user"):
		return []byte(a.username), nil
	case strings.Contains(prompt, "pass"):
		return []byte(a.password), nil
	case a.step == 1:
		return []byte(a.username), nil
	case a.step == 2:
		return []byte(a.password), nil
	}
	return nil, fmt.Errorf("unexpected AUTH LOGIN prompt %q", fromServer)
}
