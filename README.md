# Taskboard Runner

The runner for taskboard's Test Maker. It runs API test suites on your own
server, against your own services, and sends back only masked results.

> **Status: in development.** Nothing is released yet. The first release
> ships the binary, a container image, and local `test` / `validate` commands.

## How it works

1. Register a fleet in taskboard. You get a token, shown once.
2. Run `taskboard-runner` on your server with that token. It only makes
   outbound HTTPS calls to taskboard, so you open no inbound port.
3. Keep your secrets in an environment file on that server. taskboard learns
   their names, never their values.
4. The runner claims a run, calls your API, masks your secrets in everything
   it saw, and reports each case back.

## Why it is open source

It runs beside your secrets. You can read exactly what it does, reproduce a
failed run on your own machine, and report or fix a bug where you found it.

## Install

Every release tag publishes a container image and Linux binaries (amd64 and
arm64). Neither needs an account.

```sh
docker pull ghcr.io/risa-ai/taskboard-runner:<version>
```

Or download `taskboard-runner_<version>_linux_<arch>` and `SHA256SUMS` from the
release page, then check the download:

```sh
sha256sum --ignore-missing -c SHA256SUMS
```

## Build from source

```sh
go build -o taskboard-runner ./cmd/taskboard-runner
./taskboard-runner version
```

## Development

```sh
go vet ./...
gofmt -l .
go test ./...
```

CI runs the same three on every pull request.

## Security

Report anything that could leak a secret privately, through this
repository's Security tab ("Report a vulnerability"). Never post it in a
public issue.

## Licence

Apache-2.0
