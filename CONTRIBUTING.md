# Contributing

Thanks for helping. OneMailer is small on purpose: transactional mail over
SMTP, standard library only. Before a large change, open an issue to talk
it through.

## Rules the code keeps

- **No dependencies** outside the standard library.
- **No secrets in logs or errors**: never the body, a link, the recipient
  or the password.
- **TLS is never weakened**: certificates are always verified, plain SMTP
  only to loopback or the named catcher host, credentials never in clear.
- **Header values never carry CR or LF.**
- **Applications using v0.x keep compiling**: add options and fields; don't
  change existing signatures.

## Checks

```sh
gofmt -l .        # prints nothing
go vet ./...
go test -race ./...
```

Tests use `mailtest`, so they need no network. CI runs them on the
oldest supported Go (see `go.mod`) and the two latest releases.

Add a line under "Unreleased" in [CHANGELOG.md](CHANGELOG.md) for anything
a user would notice.
