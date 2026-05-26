package session

import (
	"encoding/xml"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.uber.org/atomic"
	"github.com/WarrDoge/sslcon/base"
	"github.com/WarrDoge/sslcon/proto"
	"github.com/WarrDoge/sslcon/utils"
)

var (
	Sess = &Session{}
)

type Session struct {
	SessionToken    string
	PreMasterSecret []byte

	ActiveClose bool
	CloseChan   chan struct{} // Notifies all UIs that the ConnSession has closed.
	CSess       *ConnSession
}

type stat struct {
	// be sure to use the double type when parsing
	BytesSent     uint64 `json:"bytesSent"`
	BytesReceived uint64 `json:"bytesReceived"`
}

// ConnSession used for both TLS and DTLS
type ConnSession struct {
	Sess *Session `json:"-"`

	ServerAddress string
	LocalAddress  string
	Hostname      string
	TunName       string
	VPNAddress    string // The IPv4 address of the client
	VPNMask       string // IPv4 netmask
	DNS           []string
	MTU           int
	SplitInclude  []string
	SplitExclude  []string

	DynamicSplitTunneling       bool
	DynamicSplitIncludeDomains  []string
	DynamicSplitIncludeResolved sync.Map // https://github.com/golang/go/issues/31136
	DynamicSplitExcludeDomains  []string
	DynamicSplitExcludeResolved sync.Map

	TLSCipherSuite    string
	TLSDpdTime        int // https://datatracker.ietf.org/doc/html/rfc3706
	TLSKeepaliveTime  int
	DTLSPort          string
	DTLSDpdTime       int
	DTLSKeepaliveTime int
	DTLSId            string `json:"-"` // used by the server to associate the DTLS channel with the CSTP channel
	DTLSCipherSuite   string
	Stat              *stat

	closeOnce      sync.Once           `json:"-"`
	CloseChan      chan struct{}       `json:"-"`
	PayloadIn      chan *proto.Payload `json:"-"`
	PayloadOutTLS  chan *proto.Payload `json:"-"`
	PayloadOutDTLS chan *proto.Payload `json:"-"`

	DtlsConnected *atomic.Bool
	DtlsSetupChan chan struct{} `json:"-"`
	DSess         *DtlsSession  `json:"-"`

	ResetTLSReadDead  *atomic.Bool `json:"-"`
	ResetDTLSReadDead *atomic.Bool `json:"-"`
}

type DtlsSession struct {
	closeOnce sync.Once
	CloseChan chan struct{}
}

func (sess *Session) NewConnSession(header *http.Header) *ConnSession {
	cSess := &ConnSession{
		Sess:              sess,
		LocalAddress:      base.LocalInterface.Ip4,
		Stat:              &stat{0, 0},
		closeOnce:         sync.Once{},
		CloseChan:         make(chan struct{}),
		DtlsSetupChan:     make(chan struct{}),
		PayloadIn:         make(chan *proto.Payload, 64),
		PayloadOutTLS:     make(chan *proto.Payload, 64),
		PayloadOutDTLS:    make(chan *proto.Payload, 64),
		DtlsConnected:     atomic.NewBool(false),
		ResetTLSReadDead:  atomic.NewBool(true),
		ResetDTLSReadDead: atomic.NewBool(true),
		DSess: &DtlsSession{
			closeOnce: sync.Once{},
			CloseChan: make(chan struct{}),
		},
	}
	sess.CSess = cSess

	sess.ActiveClose = false
	sess.CloseChan = make(chan struct{})

	cSess.VPNAddress = header.Get("X-CSTP-Address")
	cSess.VPNMask = header.Get("X-CSTP-Netmask")
	cSess.MTU, _ = strconv.Atoi(header.Get("X-CSTP-MTU"))
	cSess.DNS = header.Values("X-CSTP-DNS")
	// If the server sends an empty string, the slice is non-nil and IP parsing would fail.
	cSess.SplitInclude = header.Values("X-CSTP-Split-Include")
	cSess.SplitExclude = header.Values("X-CSTP-Split-Exclude")
	// debug with https://ip.900cha.com/
	// cSess.SplitExclude = append(cSess.SplitExclude, "47.243.165.103/255.255.255.255")

	cSess.TLSDpdTime, _ = strconv.Atoi(header.Get("X-CSTP-DPD"))
	cSess.TLSKeepaliveTime, _ = strconv.Atoi(header.Get("X-CSTP-Keepalive"))
	// https://datatracker.ietf.org/doc/html/draft-mavrogiannopoulos-openconnect-02#section-2.1.5.1
	cSess.DTLSId = header.Get("X-DTLS-Session-ID")
	if cSess.DTLSId == "" {
		// Compatibility with newer ocserv versions.
		cSess.DTLSId = header.Get("X-DTLS-App-ID")
	}
	cSess.DTLSPort = header.Get("X-DTLS-Port")
	cSess.DTLSDpdTime, _ = strconv.Atoi(header.Get("X-DTLS-DPD"))
	cSess.DTLSKeepaliveTime, _ = strconv.Atoi(header.Get("X-DTLS-Keepalive"))
	if base.Cfg.NoDTLS {
		cSess.DTLSCipherSuite = "Unknown"
	} else {
		cSess.DTLSCipherSuite = header.Get("X-DTLS12-CipherSuite") // The format differs before and after connection.
	}

	postAuth := header.Get("X-CSTP-Post-Auth-XML")
	if postAuth != "" {
		dtd := proto.DTD{}
		err := xml.Unmarshal([]byte(postAuth), &dtd)
		if err == nil {
			if dtd.Config.Opaque.CustomAttr.DynamicSplitIncludeDomains != "" {
				cSess.DynamicSplitIncludeDomains = strings.Split(dtd.Config.Opaque.CustomAttr.DynamicSplitIncludeDomains, ",")
				cSess.DynamicSplitTunneling = true
			} else if dtd.Config.Opaque.CustomAttr.DynamicSplitExcludeDomains != "" {
				// A trailing comma leaves the last element empty; other config errors may do the same.
				// Go has no direct container element deletion here, so leave cleanup to later consumers.
				cSess.DynamicSplitExcludeDomains = strings.Split(dtd.Config.Opaque.CustomAttr.DynamicSplitExcludeDomains, ",")
				cSess.DynamicSplitTunneling = true
			}

		}
	}

	return cSess
}

func (cSess *ConnSession) DPDTimer() {
	go func() {
		defer func() {
			base.Info("dead peer detection timer exit")
		}()
		base.Debug("TLSDpdTime:", cSess.TLSDpdTime, "TLSKeepaliveTime", cSess.TLSKeepaliveTime,
			"DTLSDpdTime", cSess.DTLSDpdTime, "DTLSKeepaliveTime", cSess.DTLSKeepaliveTime)
		// Keep this simple: probe at least every 15 seconds, with at least 5 seconds of slack.
		dpdTime := utils.Min(cSess.TLSDpdTime, cSess.DTLSDpdTime) - 5
		if dpdTime < 10 {
			dpdTime = 10
		}
		ticker := time.NewTicker(time.Duration(dpdTime) * time.Second)

		tlsDpd := proto.Payload{
			Type: 0x03,
			Data: make([]byte, 0, 8),
		}
		dtlsDpd := proto.Payload{
			Type: 0x03,
			Data: make([]byte, 0, 1),
		}

		for {
			select {
			case <-ticker.C:
				// base.Debug("dead peer detection")
				select {
				case cSess.PayloadOutTLS <- &tlsDpd:
				default:
				}
				if cSess.DtlsConnected.Load() {
					select {
					case cSess.PayloadOutDTLS <- &dtlsDpd:
					default:
					}
				}
			case <-cSess.CloseChan:
				ticker.Stop()
				return
			}
		}
	}()
}

func (cSess *ConnSession) ReadDeadTimer() {
	go func() {
		defer func() {
			base.Info("read dead timer exit")
		}()
		// Avoid resetting the read deadline on every loop iteration.
		// This is an absolute deadline; conn.Read blocks when the server is idle and resumes checking when data arrives.
		ticker := time.NewTicker(4 * time.Second)
		for range ticker.C {
			select {
			case <-cSess.CloseChan:
				ticker.Stop()
				return
			default:
				cSess.ResetTLSReadDead.Store(true)
				cSess.ResetDTLSReadDead.Store(true)
			}
		}
	}()
}

func (cSess *ConnSession) Close() {
	cSess.closeOnce.Do(func() {
		if cSess.DtlsConnected.Load() {
			cSess.DSess.Close()
		}
		close(cSess.CloseChan)
		Sess.CSess = nil

		close(Sess.CloseChan)
	})
}

func (dSess *DtlsSession) Close() {
	dSess.closeOnce.Do(func() {
		close(dSess.CloseChan)
		if Sess.CSess != nil {
			Sess.CSess.DtlsConnected.Store(false)
			Sess.CSess.DTLSCipherSuite = ""
		}
	})
}
