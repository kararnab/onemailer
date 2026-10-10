# Changelog

All notable changes to this module. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and the module
uses [semantic versioning](https://semver.org/) (before 1.0, a minor
version may change the API).

## [Unreleased]

### Fixed
- `Queue` no longer retries permanent failures: an invalid message, an SMTP
  5xx reply (`550` unknown recipient, `535` bad login) or a server refused by
  the configuration is dropped after one try with `reason=rejected` (#1).
- `NewSMTPSender` applies `LoadConfig`'s defaults and refusals to a `Config`
  built in code. Before, an empty `TLS` sent mail unencrypted to any host
  and a nil `From` panicked (#2).
- A second concurrent `Queue.Close` waits for the first to finish draining,
  or for its own context (#3).
- A message without `Kind` is logged as `mail_sent` etc., not `_sent` (#4).

### Added
- `Config.WithPassword` for configuration that does not come from the
  environment (#5).
- `OptionalPublicURL()` option for `LoadConfig`; `PUBLIC_URL` stays
  required by default (#6).
- AUTH LOGIN when the server does not offer PLAIN (Microsoft 365); `mailtest`
  can offer LOGIN with `Options.AuthMechs` (#7).
- `Message.Headers` for extra header fields such as `List-Unsubscribe` (#8).
- `Message.Attachments` (`multipart/mixed`) (#9).
- `NewSender` and `ErrMailOff`: a sender that delivers nothing when mail is
  off, logged at info as `reason=mail_off` (#10).
- `QueueConfig.OnResult`, `Result`, `Outcome`, the `Reason*` constants and
  `Queue.Len` for metrics (#11).
- `ErrPermanent`, `Permanent` and `IsPermanent`.
- Runnable examples, a "Delivery guarantees" README section (#12, #13).

### Changed
- The minimum Go version is now 1.22 (was 1.26).

## [0.1.0]

- First release: `LoadConfig`, `SMTPSender`, `Message`, `Queue`,
  `Lifetime` and `mailtest`, extracted from CodeAtlas.

[Unreleased]: https://github.com/kararnab/onemailer/compare/v0.1.0...HEAD
[0.1.0]: https://github.com/kararnab/onemailer/releases/tag/v0.1.0
