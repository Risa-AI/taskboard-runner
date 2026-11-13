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

Every call the runner makes is described in [docs/protocol.md](docs/protocol.md).

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

## Run it

Register a fleet in taskboard, then start the runner on the server beside
your services. Keep the environments file readable only by you (`chmod 600`)
and run the container as yourself, so it can read the file:

```sh
docker run -d --name taskboard-runner --restart unless-stopped \
  --user "$(id -u):$(id -g)" \
  -e TB_URL=https://taskboard.example.com \
  -e TB_FLEET_TOKEN=<the fleet's token> \
  -v "$PWD/environments.yaml:/etc/tb/environments.yaml:ro" \
  ghcr.io/risa-ai/taskboard-runner:<version>
```

With the binary: `TB_URL=... TB_FLEET_TOKEN=... taskboard-runner run --environments ./environments.yaml`.

- `TB_URL`: taskboard's address, over HTTPS (plain HTTP only to this host).
- `TB_FLEET_TOKEN`: the token taskboard showed once when the fleet was
  registered.
- `TB_SLOTS`: how many runs at once; 1 if unset.
- `--environments FILE`, else `$TB_ENVIRONMENTS`, else
  `/etc/tb/environments.yaml`: the environments file, in the format below.

The runner tells taskboard which secrets it holds (names only), heartbeats
every 30 seconds, and takes a run whenever a slot is free. It reads the
file again for every run, and tells taskboard again when the names in it
change. A secret a run needs that the file lacks, or leaves empty, fails
that run before any request, naming the secret. Stop the runner with Ctrl-C
or `docker stop`; a run in progress is reported as stopped.

## Build from source

```sh
go build -o taskboard-runner ./cmd/taskboard-runner
./taskboard-runner version
```

## Check and run a suite on your machine

These commands send nothing to taskboard.

Keep the secrets in an environments file, one entry per taskboard
environment, readable only by you (`chmod 600`):

```yaml
staging:                                           # the environment's name in taskboard
  admin_password: { env: STAGING_ADMIN_PASSWORD }  # from this machine's environment
  api_key:        { file: /run/secrets/staging_api_key }
  webhook_secret: "inline value"
```

The runner reads it from `--environments FILE`, else `$TB_ENVIRONMENTS`, else
`/etc/tb/environments.yaml`. A relative `file:` path is relative to the
environments file.

```sh
taskboard-runner validate --suite ./suite --seeds seeds.yaml --vars vars.yaml --env staging
taskboard-runner test --suite ./suite --seeds seeds.yaml --vars vars.yaml --env staging \
  --base-url https://staging.example.com
```

- `validate` checks the suite without sending a request. It prints each
  problem as `file:line: message` and exits 1 if there is any. With `--env`
  it needs the secrets' names only, not their values.
- `test` runs the suite and prints each case, then the summary. It exits 0
  when the run passed, 1 when a case failed, and 2 when no case ran. A secret
  that is unset or empty stops the run before the first request.
- `vars.yaml` holds the environment's plain variables (`name: value`) and
  `seeds.yaml` the suite's seeds. A name is a seed, a variable or a secret,
  never two.
- A form upload (`@path`) is read from the suite directory, or from
  `--fixtures DIR`.

Before anything is printed, the runner masks the environment's secrets, every
token a case captured under a name ending in `_token`, credential headers,
and the value of any JSON field named for a credential, such as
`refresh_token`. It cannot mask JSON inside a string value, a failure's
reason quoting an asserted field, or a token in a query string or a form
body, so capture tokens under names ending in `_token`.

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
