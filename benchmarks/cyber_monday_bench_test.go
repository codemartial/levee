package benchmarks

import (
	"github.com/codemartial/levee"
	"github.com/codemartial/loadgen"
)

// generateCyberMondayWorkload is an alias to the exported function for test compatibility.
func generateCyberMondayWorkload() []loadgen.LoadSpec {
	return GenerateCyberMondayWorkload()
}

// bauDegradation is an alias to the exported function for test compatibility.
func bauDegradation(hour int) float64 {
	return BauDegradation(hour)
}

func stateString(s levee.State) string {
	switch s {
	case levee.CLOSED:
		return "CLOSED"
	case levee.OPEN:
		return "OPEN"
	case levee.THROTTLED:
		return "THROTTLED"
	default:
		return "UNKNOWN"
	}
}
