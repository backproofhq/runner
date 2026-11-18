# Backproof runner

The Backproof runner proves that your latest Postgres backup actually restores. It runs in your own infrastructure, restores the backup into a throwaway container, runs checks on it and reports metadata to [Backproof](https://app.backproof.dev). Your data never leaves your machine.

> **Status:** early development. There is no release yet, and only `backproof version` works.

## Documentation

- [Configuration](docs/config.md): the settings the runner reads
- [Payload](docs/payload.md): everything the runner sends to Backproof
- [Security and trust model](docs/security.md): what we promise and how the runner keeps it
- [Changelog](CHANGELOG.md)

## Development

You need Go, Docker, [golangci-lint](https://golangci-lint.run) and [GoReleaser](https://goreleaser.com).

```sh
go build ./...                          # build
go test ./...                           # unit tests
go test -tags integration ./test/...    # integration tests
golangci-lint run                       # lint
goreleaser release --snapshot --clean   # local build, never publishes
```

Snapshot builds tag the image once per platform. To run the one for your machine:

```sh
docker tag "backproofhq/runner:dev-$(go env GOARCH)" backproofhq/runner:dev
docker run --rm backproofhq/runner:dev version
```

## Security

To report a vulnerability, see [SECURITY.md](SECURITY.md).

## License

[Apache 2.0](LICENSE)
