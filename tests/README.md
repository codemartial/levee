# Adversarial controller tests

This directory contains black-box Go integration tests for Levee's workload
boundaries and controller properties. The tests import the public package and
use `Snapshot` for assertions; they do not reach into controller internals.
The observed results and their interpretation are recorded in
[`FINDINGS.md`](FINDINGS.md).

The suite covers:

- healthy and failing traffic at very low request rates;
- mixed healthy/failing request classes behind shared versus split Levees;
- dispersed, correlated, and retry-amplified failure streams;
- a genuine healthy capacity step;
- surge trip urgency as overload severity rises;
- surge-strain relaxation after a healthy drain; and
- a 25,000-arrival seeded adversarial trace checking state, accounting, cap,
  numeric, and trigger invariants after every public operation.

Long-lived/streaming operations and workload-class switching are outside Levee's
design envelope and are not tested as expected controller behavior. Caller
completion ownership, outcome classification, and timestamp-ordering policy are
also intentionally outside this suite, matching the project's current API scope.
Explicit timestamps are used only to make controller traces deterministic.

Run the suite with:

```sh
go test -v ./tests
```

Run it with concurrency instrumentation using:

```sh
go test -race ./tests
```
