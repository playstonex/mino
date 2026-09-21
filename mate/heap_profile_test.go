package mate

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// A profile writer that silently produced an empty or unparseable file would be
// worse than none: the whole point is to stop inferring who holds the heap, and
// a zero-byte file would send the next round back to inference while looking
// like progress. So these assert the file is real and pprof-shaped.

func decodeProfileResult(t *testing.T, raw string) heapProfileResult {
	t.Helper()
	if raw == "{}" {
		t.Fatalf("WriteHeapProfileJSON returned the encode-failure sentinel")
	}
	var result heapProfileResult
	if err := json.Unmarshal([]byte(raw), &result); err != nil {
		t.Fatalf("undecodable JSON: %v (%s)", err, raw)
	}
	return result
}

func TestWriteHeapProfileProducesAParseableProfile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "heap.pprof")

	// Something worth finding in the profile, kept alive across the write.
	ballast := make([][]byte, 0, 64)
	for i := 0; i < 64; i++ {
		ballast = append(ballast, make([]byte, 128*1024))
	}

	result := decodeProfileResult(t, WriteHeapProfileJSON(path))
	if result.Error != "" {
		t.Fatalf("WriteHeapProfileJSON reported: %s", result.Error)
	}
	if result.Path != path {
		t.Errorf("Path = %q, want %q", result.Path, path)
	}
	if result.SizeBytes <= 0 {
		t.Errorf("SizeBytes = %d; an empty profile explains nothing", result.SizeBytes)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read profile: %v", err)
	}
	// pprof profiles are gzipped protobuf; the gzip magic is the cheapest check
	// that this is a profile and not a truncated or text-mode write.
	if len(data) < 2 || data[0] != 0x1f || data[1] != 0x8b {
		t.Errorf("profile does not start with gzip magic (got % x); not a pprof file",
			data[:min(len(data), 8)])
	}

	_ = ballast
}

// An unwritable path must be REPORTED, not swallowed. A caller that logs
// "profile written" on a failed write would send someone looking for a file
// that does not exist.
func TestWriteHeapProfileReportsAnUnwritablePath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "no-such-directory", "heap.pprof")

	result := decodeProfileResult(t, WriteHeapProfileJSON(path))
	if result.Error == "" {
		t.Error("Error is empty for a path that cannot be created")
	}
	if result.SizeBytes != 0 {
		t.Errorf("SizeBytes = %d for a failed write, want 0", result.SizeBytes)
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
