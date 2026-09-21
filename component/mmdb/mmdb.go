package mmdb

import (
	"sync"

	mihomoOnce "github.com/metacubex/mihomo/common/once"
	C "github.com/metacubex/mihomo/constant"
	"github.com/metacubex/mihomo/log"

	"github.com/oschwald/maxminddb-golang"
)

type databaseType = uint8

const (
	typeMaxmind databaseType = iota
	typeSing
	typeMetaV0
)

var (
	ipReader  IPReader
	asnReader ASNReader
	ipOnce    sync.Once
	asnOnce   sync.Once
)

// The loaders below DEGRADE on a missing or corrupt database rather than
// calling log.Fatalln, which exits the process.
//
// On a desktop that exit is survivable: mihomo is the whole program, the user
// sees the message and fixes the file. Inside a Network Extension it is not --
// the tunnel process dies, every connection with it, and the reason is a geo
// database that was only needed to decide ONE rule. Worse, the loaders are
// sync.Once-gated and only run on the first GEOIP/ASN match, so the exit
// happens at an arbitrary point mid-session rather than at startup: the tunnel
// comes up fine, serves traffic, and then vanishes the moment a packet reaches
// a GEOIP rule. Violet ships no .mmdb at all, so that was a live hazard.
//
// Degrading means the reader stays nil and every lookup returns no match, so a
// GEOIP rule simply never matches and traffic falls through to the next rule.
// For a proxy that is the right failure: routing a connection through the
// default path is recoverable, killing the tunnel is not.

func LoadFromBytes(buffer []byte) {
	ipOnce.Do(func() {
		mmdb, err := maxminddb.FromBytes(buffer)
		if err != nil {
			log.Errorln("Can't load mmdb: %s — GEOIP rules will not match", err.Error())
			return
		}
		ipReader = IPReader{Reader: mmdb}
		switch mmdb.Metadata.DatabaseType {
		case "sing-geoip":
			ipReader.databaseType = typeSing
		case "Meta-geoip0":
			ipReader.databaseType = typeMetaV0
		default:
			ipReader.databaseType = typeMaxmind
		}
	})
}

func Verify(path string) bool {
	instance, err := maxminddb.Open(path)
	if err == nil {
		instance.Close()
	}
	return err == nil
}

func IPInstance() IPReader {
	ipOnce.Do(func() {
		mmdbPath := C.Path.MMDB()
		log.Infoln("Load MMDB file: %s", mmdbPath)
		mmdb, err := maxminddb.Open(mmdbPath)
		if err != nil {
			log.Errorln("Can't load MMDB: %s — GEOIP rules will not match", err.Error())
			return
		}
		ipReader = IPReader{Reader: mmdb}
		switch mmdb.Metadata.DatabaseType {
		case "sing-geoip":
			ipReader.databaseType = typeSing
		case "Meta-geoip0":
			ipReader.databaseType = typeMetaV0
		default:
			ipReader.databaseType = typeMaxmind
		}
	})

	return ipReader
}

func ASNInstance() ASNReader {
	asnOnce.Do(func() {
		ASNPath := C.Path.ASN()
		log.Infoln("Load ASN file: %s", ASNPath)
		asn, err := maxminddb.Open(ASNPath)
		if err != nil {
			log.Errorln("Can't load ASN: %s — ASN lookups will return empty", err.Error())
			return
		}
		asnReader = ASNReader{Reader: asn}
	})

	return asnReader
}

func ReloadIP() {
	mihomoOnce.Reset(&ipOnce)
}

func ReloadASN() {
	mihomoOnce.Reset(&asnOnce)
}
