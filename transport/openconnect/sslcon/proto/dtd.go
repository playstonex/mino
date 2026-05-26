package proto

import "encoding/xml"

// DTD defines the XML request and response structures used by the client and server.
// https://datatracker.ietf.org/doc/html/draft-mavrogiannopoulos-openconnect-03#appendix-C.1
type DTD struct {
	XMLName              xml.Name       `xml:"config-auth"`
	Client               string         `xml:"client,attr"`                 // usually "vpn"
	Type                 string         `xml:"type,attr"`                   // request type: init, logout, auth-reply
	AggregateAuthVersion string         `xml:"aggregate-auth-version,attr"` // usually 2
	Version              string         `xml:"version"`                     // client version
	GroupAccess          string         `xml:"group-access"`                // requested address
	GroupSelect          string         `xml:"group-select"`                // selected group name
	ClientCertRequest    *struct{}      `xml:"client-cert-request"`
	SessionToken         string         `xml:"session-token"`
	Error                authError      `xml:"error"`
	Auth                 auth           `xml:"auth"`
	DeviceId             deviceId       `xml:"device-id"`
	Opaque               opaque         `xml:"opaque"`
	Capabilities         capabilities   `xml:"capabilities"`
	MacAddressList       macAddressList `xml:"mac-address-list"`
	Config               config         `xml:"config"`
}

type auth struct {
	Username string    `xml:"username"`
	Password string    `xml:"password"`
	Message  string    `xml:"message"`
	Banner   string    `xml:"banner"`
	Error    authError `xml:"error"`
	Form     form      `xml:"form"`
}

type form struct {
	Action string   `xml:"action,attr"`
	Groups []string `xml:"select>option"`
}

type authError struct {
	ID     string `xml:"id,attr"`
	Param1 string `xml:"param1,attr"`
	Param2 string `xml:"param2,attr"`
	Value  string `xml:",chardata"`
}

type deviceId struct {
	ComputerName    string `xml:"computer-name,attr"`
	DeviceType      string `xml:"device-type,attr"`
	PlatformVersion string `xml:"platform-version,attr"`
	UniqueId        string `xml:"unique-id,attr"`
	UniqueIdGlobal  string `xml:"unique-id-global,attr"`
}

type opaque struct {
	TunnelGroup string `xml:"tunnel-group"`
	AuthMethod  string `xml:"auth-method"`
	GroupAlias  string `xml:"group-alias"`
	ConfigHash  string `xml:"config-hash"`
}

type capabilities struct {
	AuthMethods []string `xml:"auth-method"`
}

type macAddressList struct {
	MacAddress string `xml:"mac-address"`
}

type config struct {
	Opaque opaque2 `xml:"opaque"`
}

type opaque2 struct {
	CustomAttr customAttr `xml:"custom-attr"`
}

type customAttr struct {
	DynamicSplitExcludeDomains string `xml:"dynamic-split-exclude-domains"`
	DynamicSplitIncludeDomains string `xml:"dynamic-split-include-domains"`
}
