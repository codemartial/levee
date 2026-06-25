// Package levee is a self-tuning circuit breaker and concurrency limiter for Go
// services.
//
// A Levee defends a service-level objective ([SLO]) instead of exposing tuning
// knobs. It tracks the error rate (an EWMA with a Wilson confidence bound), the
// downstream capacity (goodput times latency), and the number of inflight calls,
// and moves between four states:
//
//   - CLOSED: healthy; all traffic is admitted uncapped.
//   - THROTTLED: overloaded; admission is capped by an adaptive inflight limit
//     that converges toward observed capacity using multiplicative increase /
//     multiplicative decrease (MIMD).
//   - OPEN: tripped; all traffic is rejected for a cooldown that backs off
//     exponentially across repeated trips.
//   - HALF_OPEN: probing recovery after OPEN at the minimum inflight limit.
//
// # Usage
//
// The in-band API wraps a function call:
//
//	l := levee.NewLevee(levee.SLO{SuccessRate: 0.95, Timeout: 100 * time.Millisecond})
//	_, err := l.Call(func() error { return callDownstream() })
//	if errors.Is(err, levee.ErrCircuitOpen) {
//		// shed load: the breaker rejected the call without running it
//	}
//
// The out-of-band API ([Levee.Start], [Levee.Success], [Levee.Fail]) lets the
// caller control timing, for example to drive the breaker from a simulated clock
// or to instrument work that does not fit a single function call.
//
// All methods are safe for concurrent use by multiple goroutines. The breaker
// runs no background goroutines and allocates nothing on the hot path.
package levee
