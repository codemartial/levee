package levee_test

import (
	"errors"
	"fmt"
	"time"

	"github.com/codemartial/levee"
)

// Call wraps a single function call. If the breaker is shedding load
// it returns ErrCircuitOpen without ever invoking the function.
func ExampleLevee_Call() {
	l := levee.NewLevee(levee.SLO{SuccessRate: 0.95, Timeout: 100 * time.Millisecond})

	sc, err := l.Call(func() error {
		// Call the downstream dependency here.
		return nil
	})
	switch {
	case errors.Is(err, levee.ErrCircuitOpen):
		fmt.Println("shed:", sc.State)
	case err != nil:
		fmt.Println("failed:", sc.State)
	default:
		fmt.Println("ok:", sc.State)
	}
	// Output: ok: CLOSED
}

// Snapshot exposes read-only controller signals for diagnostics and metrics.
func ExampleLevee_Snapshot() {
	l := levee.NewLevee(levee.SLO{SuccessRate: 0.95, Timeout: 100 * time.Millisecond})
	s := l.Snapshot()
	fmt.Println(s.State, s.Trigger, s.Inflight, s.Capped, s.Limit)
	// Output: CLOSED NONE 0 false 0
}
