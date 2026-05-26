package vpn

import (
	"bytes"
	"crypto/tls"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"

	"github.com/WarrDoge/sslcon/auth"
	"github.com/WarrDoge/sslcon/base"
	"github.com/WarrDoge/sslcon/session"
	"github.com/WarrDoge/sslcon/utils"
	"github.com/WarrDoge/sslcon/utils/vpnc"
)

var (
	reqHeaders    = make(map[string]string)
	SkipTunSetup  bool
)

func init() {
	reqHeaders["X-CSTP-VPNAddress-Type"] = "IPv4"
	// if base.Cfg.OS == "android" || base.Cfg.OS == "ios" {
	//    reqHeaders["X-CSTP-License"] = "mobile"
	// }
}

func initTunnel() {
	mtu := base.Cfg.BaseMTU
	if mtu <= 0 {
		mtu = 1399
	}
	reqHeaders["X-CSTP-MTU"] = fmt.Sprintf("%d", mtu)
	reqHeaders["X-CSTP-Base-MTU"] = fmt.Sprintf("%d", mtu)
	// https://datatracker.ietf.org/doc/html/draft-mavrogiannopoulos-openconnect-03#section-2.1.3
	reqHeaders["Cookie"] = "webvpn=" + session.Sess.SessionToken // All supported servers expect the session token in the Cookie header.
	reqHeaders["X-CSTP-Local-VPNAddress-IP4"] = base.LocalInterface.Ip4

	// Legacy Establishment of Secondary UDP Channel https://datatracker.ietf.org/doc/html/draft-mavrogiannopoulos-openconnect-02#section-2.1.5.1
	// worker-vpn.c requires WSPCONFIG(ws)->udp_port != 0 && req->master_secret_set != 0,
	// otherwise UDP (DTLS) is disabled.
	// If dtls_psk is enabled and the cipher suite includes PSK-NEGOTIATE, which applies to ocserv only,
	// worker-http.c sets req->master_secret_set = 1 automatically.
	// In that case no manual secret is needed and DTLS negotiates automatically, but AnyConnect clients do not support it.
	session.Sess.PreMasterSecret, _ = utils.MakeMasterSecret()
	reqHeaders["X-DTLS-Master-Secret"] = hex.EncodeToString(session.Sess.PreMasterSecret) // A hex encoded pre-master secret to be used in the legacy DTLS session negotiation

	// https://gitlab.com/openconnect/ocserv/-/blob/master/src/worker-http.c#L150
	// https://github.com/openconnect/openconnect/blob/master/gnutls-dtls.c#L75
	reqHeaders["X-DTLS12-CipherSuite"] = "ECDHE-ECDSA-AES256-GCM-SHA384:ECDHE-RSA-AES256-GCM-SHA384:ECDHE-ECDSA-AES128-GCM-SHA256:ECDHE-RSA-AES128-GCM-SHA256:AES128-GCM-SHA256"
}

// SetupTunnel initiates an HTTP CONNECT command to establish a VPN
func SetupTunnel() error {
	initTunnel()

	// https://github.com/golang/go/commit/da6c168378b4c1deb2a731356f1f438e4723b8a7
	// https://github.com/golang/go/issues/17227#issuecomment-341855744
	req, _ := http.NewRequest("CONNECT", auth.Prof.Scheme+auth.Prof.HostWithPort+"/CSCOSSLC/tunnel", nil)
	utils.SetCommonHeader(req)
	for k, v := range reqHeaders {
		// req.Header.Set canonicalizes the key to title case.
		req.Header[k] = []string{v}
	}

	// Send the CONNECT request.
	err := req.Write(auth.State.Conn)
	if err != nil {
		auth.State.Conn.Close()
		return err
	}
	var resp *http.Response
	// resp.Body closed when tlsChannel exit
	resp, err = http.ReadResponse(auth.State.BufR, req)
	if err != nil {
		auth.State.Conn.Close()
		return err
	}

	if resp.StatusCode != http.StatusOK {
		auth.State.Conn.Close()
		return fmt.Errorf("tunnel negotiation failed %s", resp.Status)
	}
	// Negotiation succeeded; read the configuration returned by the server.
	// https://datatracker.ietf.org/doc/html/draft-mavrogiannopoulos-openconnect-03#section-2.1.3

	// Check debug mode first to avoid unnecessary conversions.
	// http.ReadResponse canonicalizes header keys even if the server sent them in another case.
	if base.Cfg.LogLevel == "Debug" {
		headers := make([]byte, 0)
		buf := bytes.NewBuffer(headers)
		// http.ReadResponse: Keys in the map are canonicalized (see CanonicalHeaderKey).
		// https://ron-liu.medium.com/what-canonical-http-header-mean-in-golang-2e97f854316d
		_ = resp.Header.Write(buf)
		base.Debug(buf.String())
	}

	cSess := session.Sess.NewConnSession(&resp.Header)
	cSess.ServerAddress = strings.Split(auth.State.Conn.RemoteAddr().String(), ":")[0]
	cSess.Hostname = auth.Prof.Host
	cSess.TLSCipherSuite = tls.CipherSuiteName(auth.State.Conn.ConnectionState().CipherSuite)

	if !SkipTunSetup {
		err = setupTun(cSess)
		if err != nil {
			auth.State.Conn.Close()
			cSess.Close()
			return err
		}

		err = vpnc.SetRoutes(cSess)
		if err != nil {
			auth.State.Conn.Close()
			cSess.Close()
		}
	}
	base.Info("tls channel negotiation succeeded")

	// Only continue after the interface and routes are configured successfully.
	// https://datatracker.ietf.org/doc/html/draft-mavrogiannopoulos-openconnect-03#section-2.1.4
	go tlsChannel(auth.State.Conn, auth.State.BufR, cSess, resp)

	if !base.Cfg.NoDTLS && cSess.DTLSPort != "" {
		// https://datatracker.ietf.org/doc/html/draft-mavrogiannopoulos-openconnect-03#section-2.1.5
		go dtlsChannel(cSess)
	}

	cSess.DPDTimer()
	cSess.ReadDeadTimer()

	return err
}
