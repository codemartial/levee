# Levee: Self Tuning Circuit Breaker and Concurrency Limiter

> lev·ee /ˈlevi/ _noun_
>
> An embankment built to prevent the overflow of a river or body of water; specifically: an artificial bank confining a river channel or limiting adjacent areas subject to flooding.

Levee keeps more of your business flowing under dynamic conditions than any other circuit-breaker and rate-limiter combination.


- 248 bytes memory overhead (yes, under a quarter of a kB)
- multi-million request processing capacity -- per CPU core

## What is Levee?

Levee is a self-tuning circuit breaker and concurrency-based rate limiter for Go services.

- Always watching, always adapting
- Fully self-contained, 100% in-process operation
- No external dependencies

Works as a circuit breaker on outbound requests to prevent cascading failures from degraded or faulty dependencies. Works as a rate limiter on incoming requests to prevent failure due to overload.

Levee is designed to be dead simple to integrate and take the guesswork out of configuring operational parameters.

Inspired by Hystrix from Netflix.

## Why Levee?

Circuit breakers and concurrency limiters are essential components of any distributed system. However, the operating parameters of services can change over time, both short term (e.g., due to a sudden spike in traffic) and long term (e.g., due to changes in the service's dependencies).

This means that the parameters of the circuit breaker and concurrency limiter need to be adjusted frequently to ensure optimal performance, but they're rarely updated often enough. Besides, circuit breaker tuning is done unscientifically, based on heuristics and guesswork.

This can lead to suboptimal performance, with the circuit breaker either being too aggressive (causing unnecessary service denial) or too lenient (allowing cascading failures).

Levee continuously monitors the RED metrics -- R: Requests per Second, E: Error Rate, D: Duration aka Response Time or Latency -- as well as in-flight concurrents. It computes statistical properties of these signals to adjust its operating parameters dynamically, ensuring that the circuit breaker and concurrency limiter are always optimally tuned.

Adding levee instances throughout the network can provide the resiliency benefits of a decentralised service mesh while keeping yaml-hell away.

Levee is also painstakingly designed to consume a fixed, small amount of memory, making it suitable for use in high-performance, low-latency services.

## How to use Levee?

### Basic, in-band usage:

```go
package main

import (
	"fmt"
	"time"

	"github.com/codemartial/levee"
)

func main() {
	slo := levee.SLO{
		SuccessRate: 0.95,
		Timeout:     time.Millisecond * 100,
	}

	l := levee.NewLevee(slo)

	stateChange, err := l.Call(func() error {
		// Call the upstream service
		return nil
	})

	switch stateChange.State {
	case levee.OPEN:
		fmt.Println("Circuit breaker is Open")
	case levee.THROTTLED:
		fmt.Println("Circuit breaker is Throttling")
	case levee.CLOSED:
		fmt.Println("Circuit breaker is Closed")
	}
}
```

### Advanced, out-of-band usage:

Levee can be called out-of-band so you can better organise your
code and just call Levee at the start and end of your tasks. You can
even control the timing, e.g. for stream processors that work on
event-time or ingestion-time. Here's an example:

```go
package main

import (
	"fmt"
	"time"

	"github.com/codemartial/levee"
)

func main() {
	slo := levee.SLO{
		SuccessRate: 0.95,
		Timeout:     time.Millisecond * 100,
	}

	l := levee.NewLevee(slo)

	// Check circuit state before starting the task
	start := time.Now()
	stateChange, err := l.Start(start)
	if err != nil {
		fmt.Println("Circuit is open, request rejected")
		return
	}

	// Perform the actual task
	taskErr := callUpstreamService()
	end := time.Now()
	duration := end.Sub(start)

	// Report the outcome
	if taskErr != nil {
		stateChange = l.Fail(end, duration)
	} else {
		stateChange = l.Success(end, duration)
	}

	fmt.Printf("Circuit state: %v\n", stateChange.State)
}
```


## Benchmarks

Levee is validated in a closed-loop distributed simulation where circuit-breaker
decisions shape backend load, autoscaling, and queue backpressure. Across seven
traffic and error-rate variations, Levee beats a meticulously tuned static breaker
in every one, with its widest margins under extreme overload: where a binary
open/close cannot keep up, Levee's concurrency control matches admission to
available capacity.

See [benchmarks/](benchmarks/) for the full methodology and results.

Run with (from the `benchmarks/` directory):

```
go test -v -run TestDistributedBenchmarkFirstIncident -timeout 15m
```

