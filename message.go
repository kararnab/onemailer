// Package onemailer sends transactional email over plain SMTP: password
// resets, verification links, invites. It speaks SMTP to any provider
// (Amazon SES, Postmark, SendGrid, Resend, …) and to a development catcher
// such as Mailpit, so going to production is a change of configuration
// only. See the README for the settings and the go-live checklist.
//
// The pieces: LoadConfig reads {PREFIX}MAIL_* / {PREFIX}SMTP_* settings
// and refuses inconsistent ones; SMTPSender delivers one Message with the
// configured TLS mode (certificates always verified); Queue delivers off
// the request path with retries and logs outcomes without bodies, links or
// recipients; mailtest is an in-process SMTP server for tests.
package onemailer

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"mime"
	"mime/quotedprintable"
	"net/mail"
	"strings"
	"time"
)

// Message is one email. Kind names it in logs ("password_reset"); Attrs
// are logged with its outcome and must never hold a secret (no token, no
// link, no body).
type Message struct {
	Kind    string
	To      string
	Subject string
	Text    string
	HTML    string // optional; with it the message is multipart/alternative
	Attrs   []slog.Attr
}

// ErrHeaderInjection is returned for a header value holding CR or LF.
var ErrHeaderInjection = errors.New("onemailer: a header value contains a line break")

// validate checks the recipient and the headers taken from the message.
func (m Message) validate() (*mail.Address, error) {
	for _, v := range []string{m.To, m.Subject} {
		if strings.ContainsAny(v, "\r\n") {
			return nil, ErrHeaderInjection
		}
	}
	if m.Text == "" {
		return nil, errors.New("onemailer: the message has no text part")
	}
	to, err := mail.ParseAddress(m.To)
	if err != nil {
		return nil, fmt.Errorf("onemailer: invalid recipient: %w", err)
	}
	return to, nil
}

// render builds the RFC 5322 message: the headers (Reply-To when set), then a text/plain part
// or a multipart/alternative body with text and HTML, both UTF-8 and
// quoted-printable. Lines end in CRLF.
func render(from, replyTo, to *mail.Address, m Message, now time.Time) ([]byte, error) {
	var b bytes.Buffer
	header := func(k, v string) { fmt.Fprintf(&b, "%s: %s\r\n", k, v) }
	header("From", from.String())
	if replyTo != nil {
		header("Reply-To", replyTo.String())
	}
	header("To", to.String())
	header("Subject", mime.QEncoding.Encode("utf-8", m.Subject))
	header("Date", now.Format(time.RFC1123Z))
	id, err := randomHex(16)
	if err != nil {
		return nil, err
	}
	header("Message-ID", "<"+id+"@"+domainOf(from.Address)+">")
	header("MIME-Version", "1.0")

	part := func(contentType, body string) error {
		fmt.Fprintf(&b, "Content-Type: %s; charset=UTF-8\r\n", contentType)
		b.WriteString("Content-Transfer-Encoding: quoted-printable\r\n\r\n")
		w := quotedprintable.NewWriter(&b)
		if _, err := w.Write([]byte(crlf(body))); err != nil {
			return err
		}
		if err := w.Close(); err != nil {
			return err
		}
		b.WriteString("\r\n")
		return nil
	}
	if m.HTML == "" {
		if err := part("text/plain", m.Text); err != nil {
			return nil, err
		}
		return b.Bytes(), nil
	}
	boundary, err := randomHex(12)
	if err != nil {
		return nil, err
	}
	header("Content-Type", `multipart/alternative; boundary="`+boundary+`"`)
	b.WriteString("\r\n")
	for _, p := range []struct{ typ, body string }{{"text/plain", m.Text}, {"text/html", m.HTML}} {
		b.WriteString("--" + boundary + "\r\n")
		if err := part(p.typ, p.body); err != nil {
			return nil, err
		}
	}
	b.WriteString("--" + boundary + "--\r\n")
	return b.Bytes(), nil
}

// crlf normalises line endings to CRLF.
func crlf(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	return strings.ReplaceAll(s, "\n", "\r\n")
}

func domainOf(address string) string {
	if i := strings.LastIndexByte(address, '@'); i >= 0 {
		return address[i+1:]
	}
	return "localhost"
}

func randomHex(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("onemailer: random: %w", err)
	}
	return hex.EncodeToString(buf), nil
}
