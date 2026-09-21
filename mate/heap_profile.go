package mate

import (
	"encoding/json"
	"os"
	"runtime"
	"runtime/pprof"
)

// Heap profiling, exposed so the extension can attribute its own memory.
//
// Eight rounds of this investigation inferred WHO held the heap from thread
// dumps and aggregate totals: the footprint climbed to ~50 MB with a 25 MB live
// heap and iOS SIGKILLed the process, and each theory (GC pacing, scavenger lag,
// the QUIC flow-control window, the relay buffer size, the gVisor window) was
// argued from arithmetic and then partly refuted by the next run. Three separate
// dumps landed in quic-go's ParseStreamFrame -> sync.Pool.Get -> makeslice path,
// which is a hint and not an answer: a stack dump shows where a goroutine IS,
// not what is RETAINED.
//
// A heap profile answers it directly, by allocation site, with bytes. This is
// the tool that ends the guessing, and it was available the whole time.
type heapProfileResult struct {
	Path      string `json:"path"`
	SizeBytes int64  `json:"sizeBytes"`
	Error     string `json:"error,omitempty"`
}

// WriteHeapProfileJSON writes a pprof heap profile to path and reports where it
// went and how big it is.
//
// Deliberately takes a path from the caller rather than choosing one: the only
// directory the extension can write that the app can later read is the shared
// App Group container, and Go has no business knowing that layout.
//
// `runtime.GC()` first, as pprof's own documentation advises: the profile counts
// live objects, and without a collection it also counts garbage that is merely
// unswept -- which would misattribute exactly the transient churn this is meant
// to distinguish from real retention.
func WriteHeapProfileJSON(path string) string {
	runtime.GC()

	result := heapProfileResult{Path: path}

	file, err := os.Create(path)
	if err != nil {
		result.Error = err.Error()
		return encodeHeapProfileResult(result)
	}

	if err := pprof.WriteHeapProfile(file); err != nil {
		result.Error = err.Error()
		_ = file.Close()
		return encodeHeapProfileResult(result)
	}
	if err := file.Close(); err != nil {
		result.Error = err.Error()
		return encodeHeapProfileResult(result)
	}

	if info, statErr := os.Stat(path); statErr == nil {
		result.SizeBytes = info.Size()
	}
	return encodeHeapProfileResult(result)
}

func encodeHeapProfileResult(result heapProfileResult) string {
	encoded, err := json.Marshal(result)
	if err != nil {
		return "{}"
	}
	return string(encoded)
}
