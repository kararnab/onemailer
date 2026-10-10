# OneMailer

[![ci](https://github.com/kararnab/onemailer/actions/workflows/ci.yml/badge.svg)](https://github.com/kararnab/onemailer/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/kararnab/onemailer.svg)](https://pkg.go.dev/github.com/kararnab/onemailer)
[![Go Report Card](https://goreportcard.com/badge/github.com/kararnab/onemailer)](https://goreportcard.com/report/github.com/kararnab/onemailer)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

Transactional email over plain SMTP for Go services: password resets,
verification links, invites. No provider SDK and no provider-specific code,
so the same build sends mail through a local catcher on a laptop and through
Amazon SES, Postmark, SendGrid or Resend in production; only the
configuration differs.

```
go get github.com/kararnab/onemailer
```

Standard library only; Go 1.22 or later.

## Why OneMailer

Sending a password-reset email looks like ten lines of `net/smtp`, until
you need to get the details right. OneMailer handles:

- **Safe TLS by default.** Certificates are always verified. Plain SMTP is
  refused except to loopback or one named development catcher. Credentials
  are never sent in clear.
- **No secrets in logs.** Outcomes are logged by message kind and your
  attributes, never with the body, link or recipient. `Config` never
  prints the password.
- **Configuration that fails fast.** A bad setting stops the service at
  startup with the variable's name, not at the first reset request.
- **Delivery off the request path.** A bounded queue retries temporary
  failures (4xx, network) and drops permanent ones (5xx, bad address)
  straight away.
- **A test SMTP server** (`mailtest`) with STARTTLS, implicit TLS, AUTH
  PLAIN/LOGIN and scripted failures, so your tests can cover mail too.

Compared with a provider SDK there is no lock-in: Amazon SES, Postmark,
SendGrid, Resend, Microsoft 365 or Mailpit is a configuration change.
Compared with general-purpose mail libraries it is deliberately small,
covering one recipient per transactional message.

It started as the mail package of CodeAtlas and is shared with LibraryZ.

## What's in it

| | |
|---|---|
| `LoadConfig(getenv, prefix)` | Reads `{PREFIX}MAIL_*` / `{PREFIX}SMTP_*` settings and refuses inconsistent ones, naming the variable. Never prints the password. |
| `Config` | Can also be built in code (flags, config file, secret manager), with `WithPassword`; `NewSMTPSender` applies the same defaults and refusals. |
| `NewSender` | An `SMTPSender` when mail is on; when it is off, a sender that delivers nothing (logged as `reason=mail_off`), so the wiring is the same either way. |
| `SMTPSender` | Delivers one `Message` with the configured TLS mode and AUTH PLAIN or LOGIN. Certificates are always verified; plain SMTP only to loopback or one named catcher host; credentials never sent in clear. |
| `Message` | Text, optional HTML (then `multipart/alternative`), extra headers (`List-Unsubscribe`, provider tags), attachments (then `multipart/mixed`), UTF-8 quoted-printable, `Message-ID`, `Reply-To`. Line breaks in headers are refused. |
| `Queue` | Delivers off the request path: bounded, one worker, retries temporary failures after 10 s, 1 min and 5 min, then drops and logs; permanent failures are not retried. Logs carry the message kind and your attributes, never the body, link or recipient. `OnResult` and `Len` feed metrics. |
| `IsPermanent`, `Permanent` | Tell a failure worth retrying (4xx, network) from one that is not (5xx, invalid message), for your own job system or `Sender`. |
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

sender, err := onemailer.NewSender(cfg) // delivers nothing when mail is off
if err != nil {
	log.Fatalf("mail: %v", err)
}
q := onemailer.NewQueue(sender, onemailer.QueueConfig{})
defer q.Close(shutdownCtx) // delivers what is queued, or gives up when ctx ends

q.Enqueue(onemailer.Message{
	Kind:    "password_reset", // logged as password_reset_sent / _not_sent
	To:      "reader@example.org",
	Subject: "Reset your password",
	Text:    text,
	HTML:    html,
	Attrs:   []slog.Attr{slog.String("user_id", id)}, // never a secret
})
```

Settings that don't come from the environment:

```go
from, _ := mail.ParseAddress("App <accounts@example.org>")
cfg := onemailer.Config{
	Provider: onemailer.ProviderSMTP,
	From:     from,
	Host:     "smtp.postmarkapp.com", // port 587 and starttls by default
	Username: token,
}.WithPassword(token)
sender, err := onemailer.NewSMTPSender(cfg) // refuses what LoadConfig would
```

More on [pkg.go.dev](https://pkg.go.dev/github.com/kararnab/onemailer#pkg-examples).

## Delivery guarantees

`Queue` is **in memory and at most once**. A message is lost if the process
dies while it is queued or waiting for a retry (up to about 6 minutes), and
`Close` drops (and logs) what is still pending when its context ends.
That suits password resets and verification links, where the user can ask
again. Call `Close` with a timeout from your shutdown handler.

When a message must survive a restart (receipts, invites you record as
sent), call `Sender.Send` from your own durable job system (an outbox
table, River, Asynq, SQS, …) and decide whether to retry with
`onemailer.IsPermanent(err)`.

## Configuration

Every variable carries your prefix (`LIBRARYZ_`, `CODEATLAS_`, …).

| Variable | |
|---|---|
| `MAIL_PROVIDER` | `none` (default: nothing is sent) or `smtp` |
| `PUBLIC_URL` | The origin links point at. Required with `smtp`, so a service cannot start mailing broken links; pass `onemailer.OptionalPublicURL()` to `LoadConfig` if your mail has no links. `https`, or `http` for localhost or with `ENV=development` |
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
   | Microsoft 365 | `smtp.office365.com` | 587 `starttls` | the mailbox (SMTP AUTH enabled for it) |
   | Google Workspace / Gmail | `smtp.gmail.com` | 587 `starttls` | the address, with an app password |

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

## Contributing

Issues and pull requests are welcome; see [CONTRIBUTING.md](CONTRIBUTING.md)
and the [changelog](CHANGELOG.md).

## License

MIT
