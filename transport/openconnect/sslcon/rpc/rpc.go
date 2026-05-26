package rpc

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"runtime/debug"

	"github.com/gorilla/websocket"
	"github.com/sourcegraph/jsonrpc2"
	ws "github.com/sourcegraph/jsonrpc2/websocket"
	"github.com/WarrDoge/sslcon/auth"
	"github.com/WarrDoge/sslcon/base"
	"github.com/WarrDoge/sslcon/session"
)

const (
	STATUS = iota
	CONFIG
	CONNECT
	DISCONNECT
	RECONNECT
	INTERFACE
	ABORT
	STAT
)

var (
	Clients         []*jsonrpc2.Conn
	rpcHandler      = handler{}
	connectedStr    string
	disconnectedStr string
)

type handler struct{}

func Setup() {
	go func() {
		http.HandleFunc("/rpc", rpc)
		// Exit the service or app if startup fails; listening locally does not require a working physical NIC.
		base.Fatal(http.ListenAndServe(":6210", nil))
	}()
}

func rpc(resp http.ResponseWriter, req *http.Request) {
	up := websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool {
			return true
		},
	}
	conn, err := up.Upgrade(resp, req, nil)
	if err != nil {
		base.Error(err)
		return
	}
	defer conn.Close()

	jsonStream := ws.NewObjectStream(conn)
	// base.GetBaseLogger() still points to stdout here; the current RPC library cannot swap loggers after connect.
	rpcConn := jsonrpc2.NewConn(req.Context(), jsonStream, &rpcHandler, jsonrpc2.SetLogger(base.GetBaseLogger()))
	Clients = append(Clients, rpcConn)
	<-rpcConn.DisconnectNotify()
	for i, c := range Clients {
		if c == rpcConn {
			Clients = append(Clients[:i], Clients[i+1:]...)
			base.Debug(fmt.Sprintf("client %d disconnected", i))
			break
		}
	}
}

// Handle routes requests by numeric ID.
func (_ *handler) Handle(ctx context.Context, conn *jsonrpc2.Conn, req *jsonrpc2.Request) {
	defer func() {
		if err := recover(); err != nil {
			base.Error(string(debug.Stack()))
		}
	}()

	// Request routing.
	switch req.ID.Num {
	case STAT:
		// This should not be called before a connection exists.
		if session.Sess.CSess != nil {
			_ = conn.Reply(ctx, req.ID, session.Sess.CSess.Stat)
			return
		}
		jError := jsonrpc2.Error{Code: 1, Message: disconnectedStr}
		_ = conn.ReplyWithError(ctx, req.ID, &jError)
	case STATUS:
		// This should not be called before a connection exists.
		if session.Sess.CSess != nil {
			if !base.Cfg.NoDTLS && session.Sess.CSess.DTLSPort != "" {
				// Wait for DTLS setup to finish, whether it succeeds or fails.
				<-session.Sess.CSess.DtlsSetupChan
			}

			if session.Sess.CSess != nil {
				_ = conn.Reply(ctx, req.ID, session.Sess.CSess)
				return
			}
		}

		jError := jsonrpc2.Error{Code: 1, Message: disconnectedStr}
		_ = conn.ReplyWithError(ctx, req.ID, &jError)
	case CONNECT:
		// Not connected yet at startup, or called again after another UI connects.
		if session.Sess.CSess != nil {
			_ = conn.Reply(ctx, req.ID, connectedStr)
			return
		}
		err := json.Unmarshal(*req.Params, auth.Prof)
		if err != nil {
			jError := jsonrpc2.Error{Code: 1, Message: err.Error()}
			_ = conn.ReplyWithError(ctx, req.ID, &jError)
			return
		}
		err = Connect()
		if err != nil {
			base.Error(err)
			jError := jsonrpc2.Error{Code: 1, Message: err.Error()}
			_ = conn.ReplyWithError(ctx, req.ID, &jError)
			DisConnect()
			return
		}
		connectedStr = "connected to " + auth.Prof.Host
		disconnectedStr = "disconnected from " + auth.Prof.Host
		_ = conn.Reply(ctx, req.ID, connectedStr)
		go monitor()
	case RECONNECT:
		// The UI either did not detect a network change, or already pushed updated interface info after the change.
		if session.Sess.CSess != nil {
			_ = conn.Reply(ctx, req.ID, connectedStr)
			return
		}
		err := SetupTunnel(true)
		if err != nil {
			base.Error(err)
			jError := jsonrpc2.Error{Code: 1, Message: err.Error()}
			_ = conn.ReplyWithError(ctx, req.ID, &jError)
			DisConnect()
			return
		}
		_ = conn.Reply(ctx, req.ID, connectedStr)
		go monitor()
	case DISCONNECT:
		if session.Sess.CSess != nil {
			DisConnect()
		} else {
			jError := jsonrpc2.Error{Code: 1, Message: disconnectedStr}
			_ = conn.ReplyWithError(ctx, req.ID, &jError)
		}
	case CONFIG:
		// Initialize config.
		err := json.Unmarshal(*req.Params, &base.Cfg)
		if err != nil {
			jError := jsonrpc2.Error{Code: 1, Message: err.Error()}
			_ = conn.ReplyWithError(ctx, req.ID, &jError)
			return
		}
		_ = conn.Reply(ctx, req.ID, "ready to connect")
		// Reset the logger after each client restart or config change.
		base.InitLog()
	case INTERFACE:
		err := json.Unmarshal(*req.Params, base.LocalInterface)
		if err != nil {
			jError := jsonrpc2.Error{Code: 1, Message: err.Error()}
			_ = conn.ReplyWithError(ctx, req.ID, &jError)
			return
		}
		auth.Prof.Initialized = true
		_ = conn.Reply(ctx, req.ID, "ready to connect")
	default:
		base.Debug("receive rpc call:", req)
		jError := jsonrpc2.Error{Code: 1, Message: "unknown method: " + req.Method}
		_ = conn.ReplyWithError(ctx, req.ID, &jError)
	}
}

func monitor() {
	// Mid-session DTLS teardown is not handled here.
	<-session.Sess.CloseChan
	ctx := context.Background()
	for _, conn := range Clients {
		if session.Sess.ActiveClose {
			_ = conn.Reply(ctx, jsonrpc2.ID{Num: DISCONNECT, IsString: false}, disconnectedStr)
		} else {
			_ = conn.Reply(ctx, jsonrpc2.ID{Num: ABORT, IsString: false}, disconnectedStr)
		}
	}
}
