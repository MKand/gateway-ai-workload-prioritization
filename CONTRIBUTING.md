# Contributing to Gemini Priority Quota Governor

Thank you for your interest in contributing to the Gemini Priority Quota Governor!

## License

By contributing to this repository, you agree that your contributions will be licensed under the [Apache License, Version 2.0](LICENSE).

## Prerequisites

- **OS**: Linux (verified by `make check-os`)
- **Go**: Go 1.26+
- **Protocol Buffers** (when modifying `.proto` schemas): `protoc`, `protoc-gen-go`, and `protoc-gen-go-grpc`

Verify your local toolchain:

```bash
make check-env
```

## Development Workflow

1. Create a feature branch from `main` (`feat/...`, `fix/...`, `chore/...`, `docs/...`, or `refactor/...`).
2. If you modify `proto/governor/v1/governor.proto`, regenerate the Go bindings:
   ```bash
   make proto
   ```
3. Run the unit, race, and concurrency test suites and verify both binaries build cleanly before opening a Pull Request:
   ```bash
   make test
   make test-race
   make test-concurrency
   make build
   ```
4. For changes affecting the Envoy `ext_proc` integration or `deploy/envoy/envoy.yaml`, run the containerized end-to-end test suite (requires Docker):
   ```bash
   make e2e
   ```
