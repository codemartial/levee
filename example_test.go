package levee_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/codemartial/levee"
)

// The in-band API wraps a single function call. If the breaker is shedding load
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

// The out-of-band API lets the caller control timing, for example to drive the
// breaker from a simulated clock or to instrument work that does not fit a single
// function call. Report each admitted call exactly once with Success or Fail.
func ExampleLevee_Start() {
	l := levee.NewLevee(levee.SLO{SuccessRate: 0.95, Timeout: 100 * time.Millisecond})

	now := time.Unix(0, 0)
	if _, err := l.Start(now); err != nil {
		fmt.Println("rejected:", err)
		return
	}
	// ... perform the protected work, which took 10ms and succeeded ...
	sc := l.Success(now.Add(10*time.Millisecond), 10*time.Millisecond)
	fmt.Println(sc.State)
	// Output: CLOSED
}

// A breaker can be snapshotted and restored across a process restart so the
// service resumes with its learned signals instead of a cold start. The live
// inflight count is intentionally dropped on restore.
func ExampleLevee_SaveState() {
	l := levee.NewLevee(levee.SLO{SuccessRate: 0.95, Timeout: 100 * time.Millisecond})

	snap, _ := l.SaveState()
	blob, _ := json.Marshal(snap)

	// ... process restarts; reload the snapshot bytes ...
	var restored levee.LeveeState
	_ = json.Unmarshal(blob, &restored)
	l2 := levee.RestoreState(&restored)

	fmt.Println(l2.State())
	// Output: CLOSED
}
