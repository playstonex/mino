package auth

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"runtime"
	"strings"
	"text/template"
	"time"

	"github.com/WarrDoge/sslcon/base"
	"github.com/WarrDoge/sslcon/lib"
	"github.com/WarrDoge/sslcon/proto"
	"github.com/WarrDoge/sslcon/session"
	"github.com/WarrDoge/sslcon/utils"
)

var (
	Prof    = lib.NewProfile()
	State   = lib.NewAuthState()
	DialTLS func(network, addr string, config *tls.Config) (*tls.Conn, error)
)

const (
	tplInit = iota
	tplAuthReply
)

func ensureDefaults() {
	if base.Cfg == nil {
		base.Cfg = base.NewClientConfig()
	}
	if base.LocalInterface == nil {
		base.LocalInterface = base.NewInterface()
	}
	if Prof == nil {
		Prof = lib.NewProfile()
	}
	if State == nil {
		State = lib.NewAuthState()
	}
	if len(State.ReqHeaders) == 0 {
		State.ReqHeaders = defaultRequestHeaders()
	}
	if Prof.Scheme == "" {
		Prof.Scheme = "https://"
	}
	if Prof.BasePath == "" {
		Prof.BasePath = "/"
	}
	if Prof.DeviceID == "" {
		platform := State.ReqHeaders["X-AnyConnect-Platform"]
		if platform == "" {
			platform = detectAnyConnectPlatform()
		}
		Prof.DeviceID = platform
	}
}

func defaultRequestHeaders() map[string]string {
	platform := detectAnyConnectPlatform()
	return map[string]string{
		"X-Transcend-Version":   "1",
		"X-Aggregate-Auth":      "1",
		"X-Support-HTTP-Auth":   "true",
		"X-AnyConnect-Platform": platform,
		"Accept":                "*/*",
		"Accept-Encoding":       "identity",
	}
}

func InitAuth() error {
	ensureDefaults()

	State.WebVPNCookie = ""
	config := tls.Config{
		InsecureSkipVerify: base.Cfg.InsecureSkipVerify,
	}
	if err := applyTLSOptions(&config); err != nil {
		return err
	}

	openConn := func() error {
		var err error
		if DialTLS != nil {
			State.Conn, err = DialTLS("tcp4", Prof.HostWithPort, &config)
		} else {
			State.Conn, err = tls.DialWithDialer(&net.Dialer{Timeout: 6 * time.Second}, "tcp4", Prof.HostWithPort, &config)
		}
		if err != nil {
			return err
		}
		State.BufR = bufio.NewReader(State.Conn)
		return nil
	}
	if err := openConn(); err != nil {
		return err
	}

	dtd := new(proto.DTD)
	Prof.AppVersion = base.Cfg.AgentVersion
	Prof.GroupAccess = fmt.Sprintf("%s%s%s", Prof.Scheme, requestHostForURL(), resolveRequestPath(Prof.BasePath))

	err := tplPost(tplInit, "", dtd)
	if err != nil && shouldRetryInitWithLinuxPlatform(runtime.GOOS, State.ReqHeaders["X-AnyConnect-Platform"], err) {
		base.Info("auth init returned 404 for windows-64; retrying with linux-64 compatibility profile")
		setAuthPlatform("linux-64")
		if err := openConn(); err != nil {
			return err
		}
		dtd = new(proto.DTD)
		err = tplPost(tplInit, "", dtd)
	}
	if err != nil {
		return err
	}
	if msg := dtdErrorMessage(dtd); msg != "" {
		return errors.New(msg)
	}
	if clientCertRequested(dtd) {
		closeAuthConn()
		if err := openConn(); err != nil {
			return err
		}
		dtd = new(proto.DTD)
		err = tplPost(tplInit, "", dtd)
		if err != nil {
			return err
		}
		if msg := dtdErrorMessage(dtd); msg != "" {
			return errors.New(msg)
		}
		if clientCertRequested(dtd) {
			return errors.New("gateway repeatedly requested client certificate")
		}
	}

	Prof.AuthPath = dtd.Auth.Form.Action
	if Prof.AuthPath == "" {
		Prof.AuthPath = resolveRequestPath(Prof.BasePath)
	}
	Prof.TunnelGroup = dtd.Opaque.TunnelGroup
	Prof.AuthMethod = dtd.Opaque.AuthMethod
	Prof.GroupAlias = dtd.Opaque.GroupAlias
	Prof.ConfigHash = dtd.Opaque.ConfigHash

	gps := len(dtd.Auth.Form.Groups)
	if gps != 0 && !utils.InArray(dtd.Auth.Form.Groups, Prof.Group) {
		return fmt.Errorf("available user groups are: %s", strings.Join(dtd.Auth.Form.Groups, " "))
	}

	return nil
}

func PasswordAuth() error {
	ensureDefaults()

	dtd := new(proto.DTD)
	err := tplPost(tplAuthReply, Prof.AuthPath, dtd)
	if err != nil {
		return err
	}
	if msg := dtdErrorMessage(dtd); msg != "" {
		return errors.New(msg)
	}
	if dtd.Type == "auth-request" && dtd.Auth.Error.Value == "" {
		dtd = new(proto.DTD)
		err = tplPost(tplAuthReply, Prof.AuthPath, dtd)
		if err != nil {
			return err
		}
		if msg := dtdErrorMessage(dtd); msg != "" {
			return errors.New(msg)
		}
	}
	if dtd.Type == "auth-request" {
		if dtd.Auth.Error.Value != "" {
			return fmt.Errorf(dtd.Auth.Error.Value, dtd.Auth.Error.Param1)
		}
		return errors.New(dtd.Auth.Message)
	}

	session.Sess.SessionToken = dtd.SessionToken
	if State.WebVPNCookie != "" {
		session.Sess.SessionToken = State.WebVPNCookie
	}
	if strings.TrimSpace(session.Sess.SessionToken) == "" {
		return errors.New("authentication succeeded without a session token")
	}
	base.Debug("SessionToken: [present]")
	return nil
}

var (
	parsedTemplateInit = template.Must(template.New("init").Funcs(template.FuncMap{
		"xmlEscape": xmlEscape,
	}).Parse(templateInit))
	parsedTemplateAuthReply = template.Must(template.New("auth_reply").Funcs(template.FuncMap{
		"xmlEscape": xmlEscape,
	}).Parse(templateAuthReply))
)

func tplPost(typ int, path string, dtd *proto.DTD) error {
	tplBuffer := new(bytes.Buffer)
	var err error
	if typ == tplInit {
		err = parsedTemplateInit.Execute(tplBuffer, Prof)
	} else {
		err = parsedTemplateAuthReply.Execute(tplBuffer, Prof)
	}
	if err != nil {
		return fmt.Errorf("template execute: %w", err)
	}
	if base.Cfg.LogLevel == "Debug" {
		post := tplBuffer.String()
		if typ == tplAuthReply {
			post = utils.RemoveBetween(post, "<auth>", "</auth>")
		}
		base.Debug(post)
	}

	requestPath := resolveRequestPath(path)
	targetURL := fmt.Sprintf("%s%s%s", Prof.Scheme, requestHostForURL(), requestPath)
	if Prof.SecretKey != "" {
		if strings.Contains(targetURL, "?") {
			targetURL += "&" + Prof.SecretKey
		} else {
			targetURL += "?" + Prof.SecretKey
		}
	}
	base.Debug("auth POST", targetURL)
	req, _ := http.NewRequest("POST", targetURL, tplBuffer)

	utils.SetCommonHeader(req)
	if base.Cfg.CiscoCompat {
		req.Header.Set("User-Agent", fmt.Sprintf("Open AnyConnect VPN Agent %s", Prof.AppVersion))
	}
	req.Header.Set("Content-Type", "application/xml; charset=utf-8")
	for k, v := range State.ReqHeaders {
		req.Header[k] = []string{v}
	}
	if State.Conn == nil || State.BufR == nil {
		return errors.New("auth transport is not initialized")
	}

	err = req.Write(State.Conn)
	if err != nil {
		closeAuthConn()
		return err
	}

	resp, err := http.ReadResponse(State.BufR, req)
	if err != nil {
		closeAuthConn()
		return err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		closeAuthConn()
		return err
	}
	if base.Cfg.LogLevel == "Debug" {
		base.Debug(string(body))
	}

	if resp.StatusCode == http.StatusOK {
		err = xml.Unmarshal(body, dtd)
		if dtd.Type == "complete" && dtd.SessionToken == "" {
			for _, c := range resp.Cookies() {
				if c.Name == "webvpn" {
					State.WebVPNCookie = c.Value
					break
				}
			}
		}
		return err
	}
	closeAuthConn()
	return &authHTTPStatusError{StatusCode: resp.StatusCode, Status: resp.Status}
}

type authHTTPStatusError struct {
	StatusCode int
	Status     string
}

func (e *authHTTPStatusError) Error() string {
	if e == nil {
		return "auth error"
	}
	if strings.TrimSpace(e.Status) != "" {
		return fmt.Sprintf("auth error %s", e.Status)
	}
	if e.StatusCode != 0 {
		return fmt.Sprintf("auth error %d", e.StatusCode)
	}
	return "auth error"
}

func closeAuthConn() {
	if State != nil && State.Conn != nil {
		State.Conn.Close()
		State.Conn = nil
		State.BufR = nil
	}
}

func resolveRequestPath(path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		path = strings.TrimSpace(Prof.BasePath)
	}
	if path == "" {
		return "/"
	}
	if strings.HasPrefix(path, "https://") || strings.HasPrefix(path, "http://") {
		u, err := url.Parse(path)
		if err == nil {
			path = u.EscapedPath()
			if path == "" {
				path = "/"
			}
			if u.RawQuery != "" {
				path += "?" + u.RawQuery
			}
			return path
		}
	}
	if strings.HasPrefix(path, "/") {
		return path
	}
	return "/" + path
}

func requestHostForURL() string {
	host := strings.TrimSpace(Prof.Host)
	if host != "" {
		return host
	}
	host = strings.TrimSpace(Prof.HostWithPort)
	if host != "" {
		return host
	}
	return "localhost"
}

func detectAnyConnectPlatform() string {
	platform := runtime.GOOS + "-" + runtime.GOARCH
	switch runtime.GOARCH {
	case "amd64", "arm64", "ppc64", "ppc64le", "mips64", "mips64le":
		platform = runtime.GOOS + "-64"
	}
	return platform
}

func shouldRetryInitWithLinuxPlatform(goos, platform string, err error) bool {
	if !strings.EqualFold(strings.TrimSpace(goos), "windows") {
		return false
	}
	if !strings.EqualFold(strings.TrimSpace(platform), "windows-64") {
		return false
	}
	var statusErr *authHTTPStatusError
	if !errors.As(err, &statusErr) {
		return false
	}
	return statusErr.StatusCode == http.StatusNotFound
}

func setAuthPlatform(platform string) {
	platform = strings.TrimSpace(platform)
	if platform == "" {
		return
	}
	if State.ReqHeaders == nil {
		State.ReqHeaders = defaultRequestHeaders()
	}
	State.ReqHeaders["X-AnyConnect-Platform"] = platform
	Prof.DeviceID = platform
}

func dtdErrorMessage(dtd *proto.DTD) string {
	if dtd == nil {
		return ""
	}
	if msg := strings.TrimSpace(dtd.Error.Value); msg != "" {
		return msg
	}
	if msg := strings.TrimSpace(dtd.Auth.Error.Value); msg != "" {
		return msg
	}
	return ""
}

func clientCertRequested(dtd *proto.DTD) bool {
	return dtd != nil && dtd.ClientCertRequest != nil
}

func xmlEscape(value string) string {
	var b strings.Builder
	_ = xml.EscapeText(&b, []byte(value))
	return b.String()
}

var templateInit = `<?xml version="1.0" encoding="UTF-8"?>
<config-auth client="vpn" type="init" aggregate-auth-version="2">
    <version who="vpn">{{.AppVersion}}</version>
    <device-id>{{.DeviceID}}</device-id>
    <capabilities>
        <auth-method>single-sign-on-v2</auth-method>
        <auth-method>single-sign-on-external-browser</auth-method>
    </capabilities>
    <group-access>{{xmlEscape .GroupAccess}}</group-access>
</config-auth>`

var templateAuthReply = `<?xml version="1.0" encoding="UTF-8"?>
<config-auth client="vpn" type="auth-reply" aggregate-auth-version="2">
    <version who="vpn">{{.AppVersion}}</version>
    <device-id>{{.DeviceID}}</device-id>
    <capabilities>
        <auth-method>single-sign-on-v2</auth-method>
        <auth-method>single-sign-on-external-browser</auth-method>
    </capabilities>
    <opaque is-for="sg">
        {{- if .TunnelGroup }}
        <tunnel-group>{{xmlEscape .TunnelGroup}}</tunnel-group>
        {{- end }}
        {{- if .AuthMethod }}
        <auth-method>{{xmlEscape .AuthMethod}}</auth-method>
        {{- end }}
        {{- if .GroupAlias }}
        <group-alias>{{xmlEscape .GroupAlias}}</group-alias>
        {{- end }}
        {{- if .ConfigHash }}
        <config-hash>{{xmlEscape .ConfigHash}}</config-hash>
        {{- end }}
    </opaque>
    <auth>
        <username>{{xmlEscape .Username}}</username>
        <password>{{xmlEscape .Password}}</password>
    </auth>
</config-auth>`
