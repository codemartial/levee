# Levee: Self Tuning Circuit Breaker and Concurrency Limiter

> lev·ee /ˈlevi/ _noun_
>
> 1. An embankment built to prevent the overflow of a river or body of water; specifically: an artificial bank confining a river channel or limiting adjacent areas subject to flooding.
>    - "The Mississippi River levee system stretches for hundreds of miles"
>
> 2. A continuous dike or ridge (as of earth) for confining the irrigation areas of land to be flooded.

## What is Levee?

Levee is a self-tuning circuit breaker and concurrency limiter for Go services.

A circuit breaker is a mechanism that prevents a service from being taken down due to too many errors in its dependencies (upstreams), thereby preventing a cascading failure.

A concurrency limiter is a mechanism that prevents a service from being overwhelmed by too many requests, thereby preventing a degradation of service under sudden load.

Levee combines these two mechanisms into a single, self-tuning, adaptive system that can adjust its behavior based on the observed performance of the service. This means that Levee can be used for both upstreams circuit breaking and downstream rate limiting.

## Why Levee?

Circuit breakers and concurrency limiters are essential components of any distributed system. However, the operating parameters of services can change over time, both short term (e.g., due to a sudden spike in traffic) and long term (e.g., due to changes in the service's dependencies). This means that the parameters of the circuit breaker and concurrency limiter need to be adjusted dynamically to ensure optimal performance.

Most existing circuit breaker tuning and circuit breaking, however, is done unscientifically, based on heuristics and guesswork. This can lead to suboptimal performance, with the circuit breaker either being too aggressive (causing unnecessary service degradation) or too lenient (allowing cascading failures).

Levee continuously monitors the RED metrics -- R: Requests per Second, E: Error Rate, D: Duration aka Response Time or Latency -- as well as in-flight concurrents. It computes statistical properties like mean, deviation, first derivative and percentiles over these metrics at various time scales. It uses these properties to adjust its operating parameters dynamically, ensuring that the circuit breaker and concurrency limiter are always optimally tuned.

Levee is also painstakingly designed to consume a fixed, small amount of memory, making it suitable for use in high-performance, low-latency services.

## How to use Levee?

Example usage of Levee:

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
		Warmup:      time.Second * 300,
	}

	l := levee.NewLevee(slo)

	state, err := l.Call(func() error {
		// Call the upstream service
		return nil
	})

  switch state {
  case levee.INIT:
  	fmt.Println("Circuit breaker is Initializing")
  case levee.OPEN:
  	fmt.Println("Circuit breaker is Open")
  case levee.HALF_OPEN:
  	fmt.Println("Circuit breaker is Half Open")
  case levee.CLOSED:
  	fmt.Println("Circuit breaker is Closed")
  }
}
```

## TODO
Levee is still a work in progress. Here are some of the things that need to be done:
1. Implement concurrent access
2. Implement save state and restore state capability
3. Implement SLO revisions
4. Implement state updates over channels
5. Implement timeout enforcement (currently used as a FYI)
6. Implement calling with context
7. Convenience functions for HTTP response handlers
8. Implement system load monitoring

The last one is rather tricky. There is no standard way to access the environment load in Go. The best I may be able to do is to make it Linux specific. Even that is complicated being split between VM/BM and containers.
