package onemailer_test

import (
	"context"
	"errors"
	"fmt"
	"log"
	"log/slog"
	"net/mail"
	"os"
	"time"

	"github.com/kararnab/onemailer"
)

// The environment of a service whose variables start with MYAPP_.
func ExampleLoadConfig() {
	env := map[string]string{
		"MYAPP_MAIL_PROVIDER": "smtp",
		"MYAPP_PUBLIC_URL":    "https://app.example.org",
		"MYAPP_MAIL_FROM":     "My App <accounts@example.org>",
		"MYAPP_SMTP_HOST":     "smtp.sendgrid.net",
		"MYAPP_SMTP_USERNAME": "apikey",
		"MYAPP_SMTP_PASSWORD": "not-printed",
	}
	cfg, err := onemailer.LoadConfig(func(k string) string { return env[k] }, "MYAPP_")
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(cfg)

	// A mistake names its variable.
	env["MYAPP_SMTP_TLS"] = "ssl"
	_, err = onemailer.LoadConfig(func(k string) string { return env[k] }, "MYAPP_")
	fmt.Println(err)
	// Output:
	// smtp smtp.sendgrid.net:587 (starttls, login apikey), from "My App" <accounts@example.org>
	// MYAPP_SMTP_TLS must be starttls, tls or none, got "ssl"
}

// Settings from somewhere other than the environment: a config file, flags,
// a secret manager.
func ExampleConfig_WithPassword() {
	from, _ := mail.ParseAddress("My App <accounts@example.org>")
	cfg := onemailer.Config{
		Provider: onemailer.ProviderSMTP,
		From:     from,
		Host:     "smtp.postmarkapp.com", // port 587 and starttls by default
		Username: "server-token",
	}.WithPassword("server-token")
	if _, err := onemailer.NewSMTPSender(cfg); err != nil {
		log.Fatal(err)
	}
}

// A service that sends mail off the request path. With MYAPP_MAIL_PROVIDER
// unset or none, NewSender gives a sender that delivers nothing, so the
// code is the same either way.
func ExampleNewSender() {
	cfg, err := onemailer.LoadConfig(os.Getenv, "MYAPP_")
	if err != nil {
		log.Fatal(err)
	}
	sender, err := onemailer.NewSender(cfg)
	if err != nil {
		log.Fatal(err)
	}
	q := onemailer.NewQueue(sender, onemailer.QueueConfig{})
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		q.Close(ctx) // delivers what is queued, or gives up after 10 s
	}()

	ttl := time.Hour
	q.Enqueue(onemailer.Message{
		Kind:    "password_reset", // logged as password_reset_sent / _not_sent
		To:      "reader@example.org",
		Subject: "Reset your password",
		Text:    "Open this link within " + onemailer.Lifetime(ttl) + ": " + cfg.PublicURL + "/reset#token",
		Attrs:   []slog.Attr{slog.String("user_id", "u-42")}, // never a secret
	})
}

// Counting outcomes for metrics; the callback never sees a body or address.
func ExampleQueueConfig_onResult() {
	var sender onemailer.Sender // from NewSender
	q := onemailer.NewQueue(sender, onemailer.QueueConfig{
		OnResult: func(r onemailer.Result) {
			// e.g. mailResults.WithLabelValues(r.Kind, string(r.Outcome), r.Reason).Inc()
			_ = r
		},
	})
	_ = q.Len() // queue depth, for a gauge
}

// A receipt with a PDF and an unsubscribe header.
func ExampleMessage_attachments() {
	var pdf []byte // the rendered receipt
	_ = onemailer.Message{
		Kind:    "receipt",
		To:      "Ada <ada@example.org>",
		Subject: "Your receipt",
		Text:    "Thanks for your order. Your receipt is attached.",
		Headers: map[string]string{
			"List-Unsubscribe": "<https://app.example.org/unsubscribe/abc>",
		},
		Attachments: []onemailer.Attachment{
			{Filename: "receipt.pdf", Data: pdf}, // application/pdf, from the extension
		},
	}
}

// Sending synchronously, from a job system that retries on its own.
func ExampleIsPermanent() {
	var sender onemailer.Sender // from NewSender
	err := sender.Send(context.Background(), onemailer.Message{To: "ada@example.org", Subject: "Hi", Text: "Hi"})
	switch {
	case err == nil:
	case errors.Is(err, onemailer.ErrMailOff):
		// mail is off: nothing to do
	case onemailer.IsPermanent(err):
		// bad address, refused login (SMTP 5xx): retrying will not help
	default:
		// connection trouble or SMTP 4xx: let the job system retry
	}
}

func ExampleLifetime() {
	fmt.Println(onemailer.Lifetime(time.Hour))
	fmt.Println(onemailer.Lifetime(48 * time.Hour))
	fmt.Println(onemailer.Lifetime(90 * time.Minute))
	// Output:
	// 1 hour
	// 48 hours
	// 90 minutes
}
