package mmdb

import (
	"net"
	"testing"
)

// These cover the degraded path introduced when the loaders stopped calling
// log.Fatalln on a missing database. The hazard being guarded is specific: the
// loaders are sync.Once-gated and only run on the FIRST GEOIP or ASN match, so
// inside a Network Extension the old behaviour killed the tunnel mid-session --
// after it had come up and served traffic -- the moment a packet reached a GEOIP
// rule. Violet ships no .mmdb, so that was reachable in production.
//
// A nil reader must therefore be a no-match, not a panic: swapping a clean exit
// for a nil-pointer dereference would kill the tunnel just the same.

func TestIPReaderWithNoDatabaseReturnsNoMatch(t *testing.T) {
	var reader IPReader // zero value: what a failed load leaves behind

	if reader.Available() {
		t.Error("Available() is true for a reader with no database")
	}

	// Would panic without the guard, which is the whole point.
	got := reader.LookupCode(net.ParseIP("1.1.1.1"))
	if len(got) != 0 {
		t.Errorf("LookupCode returned %v, want no match so the rule falls through", got)
	}
}

func TestASNReaderWithNoDatabaseReturnsEmpty(t *testing.T) {
	var reader ASNReader

	if reader.Available() {
		t.Error("Available() is true for a reader with no database")
	}

	// LookupASN reads r.Metadata in its switch, so this panics on the first
	// line without the guard rather than at the lookup itself.
	asn, org := reader.LookupASN(net.ParseIP("1.1.1.1"))
	if asn != "" || org != "" {
		t.Errorf("LookupASN returned (%q, %q), want empty", asn, org)
	}
}

// Every databaseType must be a no-match when the reader is nil. Without the
// early return, typeMaxmind/typeSing/typeMetaV0 each reach r.Lookup on a nil
// *maxminddb.Reader, and an unrecognised value reaches the panic in the default
// branch.
func TestNoDatabaseIsSafeForEveryDatabaseType(t *testing.T) {
	for _, dbType := range []databaseType{typeMaxmind, typeSing, typeMetaV0, 99} {
		reader := IPReader{databaseType: dbType}
		got := reader.LookupCode(net.ParseIP("8.8.8.8"))
		if len(got) != 0 {
			t.Errorf("databaseType %d: LookupCode returned %v, want no match", dbType, got)
		}
	}
}

// IPInstance must hand back a usable zero value rather than exiting when the
// database is absent. This is the call GEOIP rules make.
func TestIPInstanceDoesNotExitWhenTheDatabaseIsMissing(t *testing.T) {
	// The test binary has no MMDB at C.Path.MMDB(), so this exercises the
	// failure branch. Reaching the next line at all is the assertion: the old
	// code called log.Fatalln here and the process would be gone.
	reader := IPInstance()

	if reader.Available() {
		t.Skip("a database is present in this environment; the missing-database branch was not exercised")
	}
	if got := reader.LookupCode(net.ParseIP("1.1.1.1")); len(got) != 0 {
		t.Errorf("LookupCode returned %v on a missing database, want no match", got)
	}
}
