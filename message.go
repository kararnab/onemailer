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
// recipients; mailtest is an in-process SMTP server for tests. NewSender
// gives a sender that delivers nothing when mail is off, and IsPermanent
// tells a failure worth retrying from one that is not.
package onemailer

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"mime"
	"mime/quotedprintable"
	"net/mail"
	"path/filepath"
	"slices"
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

	// Headers are extra header fields, such as List-Unsubscribe or a
	// provider's tag header (X-SES-CONFIGURATION-SET, X-PM-Tag). The
	// headers the package writes itself (From, To, Subject, Date,
	// Message-ID, Reply-To, MIME-Version, Content-*) and Cc/Bcc are refused.
	Headers map[string]string
	// Attachments, when present, make the message multipart/mixed. Each is
	// held in memory until sent, queued messages included.
	Attachments []Attachment
}

// Attachment is one file sent with a message.
type Attachment struct {
	Filename string
	// ContentType is the media type, such as "application/pdf"; when
	// empty it is guessed from the file name's extension.
	ContentType string
	Data        []byte
}

// ErrHeaderInjection is returned for a header value holding CR or LF.
var ErrHeaderInjection = errors.New("onemailer: a header value contains a line break")

// reservedHeaders are written by render, or would change the recipients.
var reservedHeaders = []string{
	"from", "to", "cc", "bcc", "reply-to", "subject", "date", "message-id", "mime-version",
	"sender", "return-path",
}

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
	for k, v := range m.Headers {
		if strings.ContainsAny(k+v, "\r\n") {
			return nil, ErrHeaderInjection
		}
		if !headerName(k) {
			return nil, fmt.Errorf("onemailer: invalid header name %q", k)
		}
		if l := strings.ToLower(k); slices.Contains(reservedHeaders, l) || strings.HasPrefix(l, "content-") {
			return nil, fmt.Errorf("onemailer: header %s is set by onemailer, not by Message.Headers", k)
		}
	}
	for _, a := range m.Attachments {
		if a.Filename == "" || strings.ContainsFunc(a.Filename+a.ContentType, func(r rune) bool { return r < ' ' || r == 0x7f }) {
			return nil, fmt.Errorf("onemailer: invalid attachment name %q", a.Filename)
		}
		if _, err := attachmentHeaders(a); err != nil {
			return nil, err
		}
	}
	return to, nil
}

// headerName: printable ASCII except colon (RFC 5322 field-name).
func headerName(k string) bool {
	if k == "" {
		return false
	}
	for i := 0; i < len(k); i++ {
		if k[i] < 33 || k[i] > 126 || k[i] == ':' {
			return false
		}
	}
	return true
}

// render builds the RFC 5322 message: the headers (Reply-To and extra
// headers when set), then a text/plain part or a multipart/alternative body
// with text and HTML, both UTF-8 and quoted-printable, wrapped with any
// attachments in multipart/mixed. Lines end in CRLF.
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
	names := make([]string, 0, len(m.Headers))
	for k := range m.Headers {
		names = append(names, k)
	}
	slices.Sort(names)
	for _, k := range names {
		header(k, mime.QEncoding.Encode("utf-8", m.Headers[k]))
	}
	header("MIME-Version", "1.0")

	if len(m.Attachments) == 0 {
		if err := writeBody(&b, m); err != nil {
			return nil, err
		}
		return b.Bytes(), nil
	}
	mixed, err := randomHex(12)
	if err != nil {
		return nil, err
	}
	header("Content-Type", `multipart/mixed; boundary="`+mixed+`"`)
	b.WriteString("\r\n--" + mixed + "\r\n")
	if err := writeBody(&b, m); err != nil {
		return nil, err
	}
	for _, a := range m.Attachments {
		h, err := attachmentHeaders(a)
		if err != nil {
			return nil, err
		}
		b.WriteString("--" + mixed + "\r\n" + h + "Content-Transfer-Encoding: base64\r\n\r\n")
		enc := base64.StdEncoding.EncodeToString(a.Data)
		for len(enc) > 76 {
			b.WriteString(enc[:76] + "\r\n")
			enc = enc[76:]
		}
		b.WriteString(enc + "\r\n")
	}
	b.WriteString("--" + mixed + "--\r\n")
	return b.Bytes(), nil
}

// writeBody writes the text part, or the text and HTML alternatives: its
// Content-Type header, a blank line, then the content.
func writeBody(b *bytes.Buffer, m Message) error {
	part := func(contentType, body string) error {
		fmt.Fprintf(b, "Content-Type: %s; charset=UTF-8\r\n", contentType)
		b.WriteString("Content-Transfer-Encoding: quoted-printable\r\n\r\n")
		w := quotedprintable.NewWriter(b)
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
		return part("text/plain", m.Text)
	}
	boundary, err := randomHex(12)
	if err != nil {
		return err
	}
	b.WriteString(`Content-Type: multipart/alternative; boundary="` + boundary + "\"\r\n\r\n")
	for _, p := range []struct{ typ, body string }{{"text/plain", m.Text}, {"text/html", m.HTML}} {
		b.WriteString("--" + boundary + "\r\n")
		if err := part(p.typ, p.body); err != nil {
			return err
		}
	}
	b.WriteString("--" + boundary + "--\r\n")
	return nil
}

// attachmentHeaders returns the Content-Type and Content-Disposition lines
// for a; a non-ASCII file name is encoded per RFC 2231.
func attachmentHeaders(a Attachment) (string, error) {
	ct := a.ContentType
	if ct == "" {
		ct = mime.TypeByExtension(filepath.Ext(a.Filename))
	}
	if ct == "" {
		ct = "application/octet-stream"
	}
	mediaType, params, err := mime.ParseMediaType(ct)
	if err != nil {
		return "", fmt.Errorf("onemailer: attachment %q: invalid content type %q", a.Filename, ct)
	}
	typ := mime.FormatMediaType(mediaType, params)
	disp := mime.FormatMediaType("attachment", map[string]string{"filename": a.Filename})
	if typ == "" || disp == "" {
		return "", fmt.Errorf("onemailer: attachment %q: cannot encode its headers", a.Filename)
	}
	return "Content-Type: " + typ + "\r\nContent-Disposition: " + disp + "\r\n", nil
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
