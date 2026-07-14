# Contributing

Use Go 1.25 or 1.26 and keep `vendor/` synchronized with `go.mod` and `go.sum`.
Before opening a pull request, run:

```bash
go mod tidy
go mod vendor
gofmt -w cmd internal tools
go test -race ./...
go vet ./...
```

Changes to transfer semantics must include tests proving whether destination
deletions can occur. Changes to authentication, secret storage, mount helpers,
or service generation must include failure-path tests and must not put secrets
in process arguments or logs.

Release tags are created only from a clean, reviewed `main` commit. The release
workflow produces four archives plus native packages, signed checksums, SPDX
SBOMs, and GitHub build attestations.
