package onemailer

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/smtp"
	"net/textproto"
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

// NewSMTPSender returns a sender for an smtp configuration.
func NewSMTPSender(cfg Config) (*SMTPSender, error) {
	if !cfg.Enabled() {
		return nil, fmt.Errorf("onemailer: %sMAIL_PROVIDER is none", cfg.Prefix)
	}
	return &SMTPSender{cfg: cfg, now: time.Now}, nil
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
		return err
	}
	data, err := render(s.cfg.From, s.cfg.ReplyTo, to, m, s.now())
	if err != nil {
		return err
	}
	if s.cfg.TLS == TLSNone && !s.cfg.plainAllowed() {
		return errors.New("onemailer: plain SMTP is allowed only to a loopback host or the allowed plain host")
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
			return fmt.Errorf("onemailer: the server does not offer STARTTLS (%sSMTP_TLS=starttls)", s.cfg.Prefix)
		}
		if err := c.StartTLS(tlsConfig); err != nil {
			return fmt.Errorf("onemailer: STARTTLS: %w", err)
		}
	}
	if s.cfg.Username != "" {
		// net/smtp's PLAIN refuses to send credentials without TLS unless the
		// host is localhost: credentials never cross the wire in clear.
		if err := c.Auth(smtp.PlainAuth("", s.cfg.Username, s.cfg.password, s.cfg.Host)); err != nil {
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
