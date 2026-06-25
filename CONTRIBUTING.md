# Contributing to Levee

Thanks for your interest in improving Levee. This guide covers how to build,
test, and submit changes.

## Prerequisites

- Go matching the version in `go.mod` (currently 1.25.4) or newer.
- `golangci-lint` v2 for linting:

  ```
  curl -sSfL https://raw.githubusercontent.com/golangci/golangci-lint/HEAD/install.sh \
    | sh -s -- -b "$(go env GOPATH)/bin" v2.12.2
  ```

- `govulncheck` for the vulnerability scan:

  ```
  go install golang.org/x/vuln/cmd/govulncheck@latest
  ```

## Common tasks

The `Makefile` wraps the everyday commands; run `make help` to list them.

| Command           | What it does                                              |
| ----------------- | --------------------------------------------------------- |
| `make test`       | Run the unit tests.                                       |
| `make test-race`  | Run the tests under `-race` and write `coverage.out`.     |
| `make test-amd64` | Run the tests as amd64 (see "Architectures" below).       |
| `make cover`      | Print per-function coverage.                              |
| `make fuzz`       | Fuzz each target briefly (`FUZZTIME=2m make fuzz`).       |
| `make lint`       | Run golangci-lint.                                        |
| `make vuln`       | Run govulncheck.                                          |
| `make bench`      | Run the throughput micro-benchmark.                       |
| `make ci`         | Run the full local gate (vet, lint, race tests, vuln).    |

Run `make ci` before opening a pull request; it mirrors what CI enforces.

## Architectures

Levee does floating-point math whose conversion to integers can differ between
CPU architectures. A real regression once shipped because the admission cap
behaved differently on amd64 than on arm64 and validation had drifted to arm64
only. CI now runs the full race-enabled suite on both amd64 and arm64.

Please reproduce both locally before submitting changes that touch numeric or
admission logic. On Apple silicon you can run the amd64 suite through Rosetta:

```
make test-amd64
```

## Tests

- Keep new behaviour covered by a unit test. Prefer the synthetic-clock style
  used in `levee_test.go` so tests stay deterministic and fast.
- Numeric helpers should have reference-value tests (`numerics_test.go`) that
  hold on both architectures.
- State-machine changes should preserve the invariants in `invariants_test.go`
  and the fuzz properties in `levee_fuzz_test.go`.

## Benchmarks

The `benchmarks/` directory is a separate module with a closed-loop simulation
suite. It is not run in PR CI because the full sims take a long time; run it
locally when you change control behaviour. See `benchmarks/` for entry points.

## Pull requests

- Keep the change focused and the diff small.
- Update `CHANGES.md` and any affected godoc when behaviour changes.
- Make sure `make ci` passes.
