# OneMailer

Transactional email over plain SMTP for Go services: password resets,
verification links, invites. No provider SDK and no provider-specific code,
so the same build sends mail through a local catcher on a laptop and through
Amazon SES, Postmark, SendGrid or Resend in production; only the
configuration differs.

```
go get github.com/kararnab/onemailer
```

Standard library only. Extracted from CodeAtlas's mail package, where it
delivers password-reset and invite emails, so that LibraryZ and CodeAtlas
share one implementation.

## What's in it

| | |
|---|---|
| `LoadConfig(getenv, prefix)` | Reads `{PREFIX}MAIL_*` / `{PREFIX}SMTP_*` settings and refuses inconsistent ones, naming the variable. Never prints the password. |
| `SMTPSender` | Delivers one `Message` with the configured TLS mode. Certificates are always verified; plain SMTP only to loopback or one named catcher host; credentials never sent in clear. |
| `Message` | Text, optional HTML (then `multipart/alternative`), UTF-8 quoted-printable, `Message-ID`, `Reply-To`. Header values with line breaks are refused. |
| `Queue` | Delivers off the request path: bounded, one worker, retries after 10 s, 1 min and 5 min, then drops and logs. Logs carry the message kind and your attributes, never the body, link or recipient. |
| `Lifetime` | Words a link lifetime for email copy ("1 hour", "30 minutes"). |
| `mailtest` | An in-process SMTP server for tests: plain, STARTTLS or implicit TLS, optional AUTH, scripted failures. |

Templates and per-account sending caps belong to the application: they
depend on its copy and its database.

## Use

```go
cfg, err := onemailer.LoadConfig(os.Getenv, "LIBRARYZ_")
if err != nil {
	log.Fatalf("mail: %v", err) // names the variable, e.g. LIBRARYZ_SMTP_HOST
}
log.Printf("mail: %s", cfg) // "smtp smtp.example.org:587 (starttls, login apikey), from …"

if cfg.Enabled() {
	sender, _ := onemailer.NewSMTPSender(cfg)
	q := onemailer.NewQueue(sender, onemailer.QueueConfig{})
	defer q.Close(context.Background())

	q.Enqueue(onemailer.Message{
		Kind:    "password_reset", // logged as password_reset_sent / _not_sent
		To:      "reader@example.org",
		Subject: "Reset your password",
		Text:    text,
		HTML:    html,
		Attrs:   []slog.Attr{slog.String("user_id", id)}, // never a secret
	})
}
```

## Configuration

Every variable carries your prefix (`LIBRARYZ_`, `CODEATLAS_`, …).

| Variable | |
|---|---|
| `MAIL_PROVIDER` | `none` (default: nothing is sent) or `smtp` |
| `PUBLIC_URL` | The origin links point at. Required with `smtp`. `https`, or `http` for localhost or with `ENV=development` |
| `MAIL_FROM` | A real, named sender: `App <accounts@example.org>`, not `no-reply@` (people reply to reset emails, and filters score `no-reply` lower) |
| `MAIL_REPLY_TO` | Optional; an inbox someone reads |
| `SMTP_HOST` | Required with `smtp` |
| `SMTP_PORT` | Default `587` |
| `SMTP_TLS` | `starttls` (default), `tls` (implicit, usually 465) or `none` |
| `SMTP_ALLOW_PLAIN_HOST` | The one non-loopback host allowed with `SMTP_TLS=none`, for a catcher on a container network (`mailpit`) |
| `SMTP_USERNAME` | When the server requires a login |
| `SMTP_PASSWORD_FILE` | Preferred; or `SMTP_PASSWORD`. At most one |

## Development: Mailpit

[Mailpit](https://mailpit.axllent.org) keeps every message and shows it in a
browser; nothing leaves the machine. In Docker Compose:

```yaml
mailpit:
  image: axllent/mailpit:v1.31.4@sha256:b68349e3a014b90c5610bfb26b2ae36f3892d7b8cf25ee140c6c71c98d2fcf48
  ports: ["8025:8025"]   # inbox at http://localhost:8025
```

and point the service at it:

```sh
{P}MAIL_PROVIDER=smtp  {P}SMTP_HOST=mailpit  {P}SMTP_PORT=1025
{P}SMTP_TLS=none       {P}SMTP_ALLOW_PLAIN_HOST=mailpit
{P}MAIL_FROM='App <accounts@app.localhost>'  {P}PUBLIC_URL=http://localhost:8081
```

End-to-end tests can read links back from `GET http://localhost:8025/api/v1/messages`.

## Going to production

Code changes: none.

1. **Pick a provider and verify the sending domain.** Any SMTP provider works:

   | Provider | Host (check the provider's docs) | Port / TLS | Username |
   |---|---|---|---|
   | Amazon SES | `email-smtp.<region>.amazonaws.com` | 587 `starttls` | the SMTP credential's user |
   | Postmark | `smtp.postmarkapp.com` | 587 `starttls` | server API token |
   | SendGrid | `smtp.sendgrid.net` | 587 `starttls` | `apikey` |
   | Resend | `smtp.resend.com` | 465 `tls` | `resend` |

   Add the DNS records the provider gives you: **SPF**, **DKIM**, and
   **DMARC** (start with `p=none` and a report address). Without SPF and
   DKIM, mail goes to spam or is refused. SES starts new accounts in a
   sandbox that only mails verified addresses.
2. **Set the environment** above, with `SMTP_PASSWORD_FILE` pointing at a
   secret file. The service should refuse to start on a bad setting.
3. **Send a test message** from the production host with your service's
   send-test command (build `Message` by hand and call `SMTPSender.Send`
   synchronously; print `SMTPCode(err)` on failure). Check the received
   headers for `spf=pass`, `dkim=pass`, `dmarc=pass`.
4. **Exercise one real flow** (a password reset) and confirm the logs show
   the kind and no link or token.

## License

MIT
