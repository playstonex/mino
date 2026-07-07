package inbound_test

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strconv"
	"testing"
	"time"

	"github.com/metacubex/mihomo/adapter/outbound"
	"github.com/metacubex/mihomo/listener/inbound"

	"github.com/stretchr/testify/require"
)

func TestInboundVMess_MKCP_V2RayInterop(t *testing.T) {
	if skip, _ := strconv.ParseBool(os.Getenv("SKIP_INTEROP_TEST")); skip {
		t.Skip("SKIP_INTEROP_TEST is set")
	}

	v2rayBin := tlsMirrorInteropV2RayBinary(t)
	mkcpInteropTestCase(t, v2rayBin, "default", "", "")
	mkcpInteropTestCase(t, v2rayBin, "seed", "mihomo-mkcp-interop", "")
	mkcpInteropTestCase(t, v2rayBin, "header srtp", "", "srtp")
}

func mkcpInteropTestCase(t *testing.T, v2rayBin, name, seed, header string) {
	t.Run(name+"/mihomo client to v2ray server", func(t *testing.T) {
		echoAddr := startTLSMirrorInteropEcho(t)
		v2rayPort := mkcpInteropReserveUDPPort(t)
		config := mkcpInteropServerConfig(t, v2rayPort.Port(), userUUID, seed, header)

		startMKCPInteropV2Ray(t, v2rayBin, config, v2rayPort.Release)

		out, err := outbound.NewVmess(outbound.VmessOption{
			Name:     "vmess_mkcp_v2ray_server",
			Server:   "127.0.0.1",
			Port:     v2rayPort.Port(),
			UUID:     userUUID,
			Cipher:   "auto",
			Network:  "mkcp",
			MKCPOpts: outbound.MKCPOptions{Seed: seed, Header: header},
		})
		require.NoError(t, err)
		t.Cleanup(func() { _ = out.Close() })

		tlsMirrorInteropRoundTripWithRetry(t, func() (net.Conn, error) {
			return out.DialContext(context.Background(), tlsMirrorInteropMetadata(t, echoAddr))
		}, 128*1024)
	})

	t.Run(name+"/v2ray client to mihomo server", func(t *testing.T) {
		echoAddr := startTLSMirrorInteropEcho(t)
		v2rayPort := tlsMirrorInteropReservePort(t)

		in, err := inbound.NewVmess(&inbound.VmessOption{
			BaseOption: inbound.BaseOption{
				NameStr: "vmess_mkcp_v2ray_client",
				Listen:  "127.0.0.1",
				Port:    "0",
			},
			Users: []inbound.VmessUser{
				{Username: "test", UUID: userUUID},
			},
			MKCPConfig: inbound.MKCPConfig{
				Enable: true,
				Seed:   seed,
				Header: header,
			},
		})
		require.NoError(t, err)

		tunnel := tlsMirrorInteropDirectTunnel(t)
		require.NoError(t, in.Listen(tunnel))
		t.Cleanup(func() { _ = in.Close() })
		inboundPort := tlsMirrorInteropParsePort(t, tlsMirrorInteropPort(in.Address()))

		config := mkcpInteropClientConfig(t, v2rayPort.Port(), inboundPort, tlsMirrorInteropPort(echoAddr), userUUID, seed, header)
		startTLSMirrorInteropV2Ray(t, v2rayBin, config, v2rayPort, net.JoinHostPort("127.0.0.1", fmt.Sprint(v2rayPort.Port())))

		tlsMirrorInteropRoundTripWithRetry(t, func() (net.Conn, error) {
			return net.Dial("tcp", net.JoinHostPort("127.0.0.1", fmt.Sprint(v2rayPort.Port())))
		}, 128*1024)
	})
}

func mkcpInteropServerConfig(t *testing.T, listenPort int, userID, seed, header string) []byte {
	t.Helper()
	config := tlsMirrorInteropBaseConfig()
	config["inbounds"] = []any{map[string]any{
		"protocol": "vmess",
		"listen":   "127.0.0.1",
		"port":     listenPort,
		"settings": map[string]any{
			"users": []string{userID},
		},
		"streamSettings": mkcpInteropStreamConfig(seed, header),
	}}
	config["outbounds"] = []any{tlsMirrorInteropDirectOutbound()}
	return tlsMirrorInteropMarshalJSONConfig(t, config)
}

func mkcpInteropClientConfig(t *testing.T, listenPort, serverPort int, targetPort string, userID, seed, header string) []byte {
	t.Helper()
	targetPortValue := tlsMirrorInteropParsePort(t, targetPort)
	config := tlsMirrorInteropBaseConfig()
	config["inbounds"] = []any{map[string]any{
		"protocol": "dokodemo-door",
		"listen":   "127.0.0.1",
		"port":     listenPort,
		"settings": map[string]any{
			"address":  "127.0.0.1",
			"port":     targetPortValue,
			"networks": "tcp",
		},
	}}
	config["outbounds"] = []any{
		map[string]any{
			"protocol":       "vmess",
			"streamSettings": mkcpInteropStreamConfig(seed, header),
			"settings": map[string]any{
				"address": "127.0.0.1",
				"port":    serverPort,
				"uuid":    userID,
			},
		},
	}
	return tlsMirrorInteropMarshalJSONConfig(t, config)
}

func mkcpInteropStreamConfig(seed, header string) map[string]any {
	return map[string]any{
		"transport":         "kcp",
		"transportSettings": mkcpInteropTransportSettings(seed, header),
	}
}

func mkcpInteropTransportSettings(seed, header string) map[string]any {
	settings := map[string]any{}
	if seed == "" {
		if header == "" {
			return settings
		}
	} else {
		settings["seed"] = map[string]any{
			"seed": seed,
		}
	}
	if header != "" {
		settings["headerConfig"] = mkcpInteropHeaderConfig(header)
	}
	return settings
}

func mkcpInteropHeaderConfig(header string) map[string]any {
	typeName := map[string]string{
		"srtp":         "v2ray.core.transport.internet.headers.srtp.Config",
		"utp":          "v2ray.core.transport.internet.headers.utp.Config",
		"wechat-video": "v2ray.core.transport.internet.headers.wechat.VideoConfig",
		"dtls":         "v2ray.core.transport.internet.headers.tls.PacketConfig",
		"wireguard":    "v2ray.core.transport.internet.headers.wireguard.WireguardConfig",
	}[header]
	return map[string]any{
		"@type": "types.v2fly.org/" + typeName,
	}
}

func startMKCPInteropV2Ray(t *testing.T, v2rayBin string, config []byte, release func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, v2rayBin, "run", "-format=jsonv5")
	var output bytes.Buffer
	cmd.Stdin = bytes.NewReader(config)
	cmd.Stdout = &output
	cmd.Stderr = &output
	release()
	require.NoError(t, cmd.Start())
	done := make(chan error, 1)
	go func() {
		done <- cmd.Wait()
	}()
	waited := false
	t.Cleanup(func() {
		cancel()
		if !waited {
			<-done
		}
		if t.Failed() {
			t.Log(output.String())
		}
	})
	select {
	case err := <-done:
		waited = true
		require.NoError(t, err, output.String())
		t.Fatalf("v2ray exited before mKCP interop test started\n%s", output.String())
	case <-time.After(300 * time.Millisecond):
	}
}

type mkcpInteropReservedPacketPort struct {
	pc   net.PacketConn
	port int
}

func mkcpInteropReserveUDPPort(t *testing.T) *mkcpInteropReservedPacketPort {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	require.NoError(t, err)
	port := pc.LocalAddr().(*net.UDPAddr).Port
	reserved := &mkcpInteropReservedPacketPort{pc: pc, port: port}
	t.Cleanup(reserved.Release)
	return reserved
}

func (p *mkcpInteropReservedPacketPort) Port() int {
	return p.port
}

func (p *mkcpInteropReservedPacketPort) Release() {
	if p.pc != nil {
		_ = p.pc.Close()
		p.pc = nil
	}
}
