package vpn

import (
	"bufio"
	"crypto/tls"
	"encoding/binary"
	"net/http"
	"time"

	"github.com/WarrDoge/sslcon/base"
	"github.com/WarrDoge/sslcon/proto"
	"github.com/WarrDoge/sslcon/session"
)

// Reuse the existing tls.Conn and its corresponding bufio.Reader.
func tlsChannel(conn *tls.Conn, bufR *bufio.Reader, cSess *session.ConnSession, resp *http.Response) {
	defer func() {
		base.Info("tls channel exit")
		resp.Body.Close()
		_ = conn.Close()
		cSess.Close()
	}()
	var (
		err           error
		bytesReceived int
		dataLen       uint16
		dead          = time.Duration(cSess.TLSDpdTime+5) * time.Second
	)

	go payloadOutTLSToServer(conn, cSess)

	// Step 21 serverToPayloadIn
	// Read server packets, normalize their format, and push them into cSess.PayloadIn.
	for {
		// Refresh the read deadline.
		if cSess.ResetTLSReadDead.Load() {
			_ = conn.SetReadDeadline(time.Now().Add(dead))
			cSess.ResetTLSReadDead.Store(false)
		}

		pl := getPayloadBuffer()                // Take a buffer from the pool; payloadInToTun returns it later.
		bytesReceived, err = bufR.Read(pl.Data) // Blocks while the server has no data.
		if err != nil {
			base.Error("tls server to payloadIn error:", err)
			return
		}

		// base.Debug("tls server to payloadIn", "Type", pl.Data[6])
		// https://datatracker.ietf.org/doc/html/draft-mavrogiannopoulos-openconnect-03#section-2.2
		switch pl.Data[6] {
		case 0x00: // DATA
			// base.Debug("tls receive DATA")
			// Read the framed payload length.
			dataLen = binary.BigEndian.Uint16(pl.Data[4:6])
			// Remove the CSTP header.
			copy(pl.Data, pl.Data[8:8+dataLen])
			// Trim the slice to the payload length.
			pl.Data = pl.Data[:dataLen]

			select {
			case cSess.PayloadIn <- pl:
			case <-cSess.CloseChan:
				return
			}
		case 0x04:
			base.Debug("tls receive DPD-RESP")
		case 0x03: // DPD-REQ
			pl.Type = 0x04
			select {
			case cSess.PayloadOutTLS <- pl:
			case <-cSess.CloseChan:
				return
			}
		}
		cSess.Stat.BytesReceived += uint64(bytesReceived)
	}
}

// payloadOutTLSToServer Step 4
func payloadOutTLSToServer(conn *tls.Conn, cSess *session.ConnSession) {
	defer func() {
		base.Info("tls payloadOut to server exit")
		_ = conn.Close()
		cSess.Close()
	}()

	var (
		err       error
		bytesSent int
		pl        *proto.Payload
	)

	for {
		select {
		case pl = <-cSess.PayloadOutTLS:
		case <-cSess.CloseChan:
			return
		}

		// base.Debug("tls payloadOut to server", "Type", pl.Type)
		if pl.Type == 0x00 {
			// Extend to make room for the CSTP header.
			l := len(pl.Data)
			pl.Data = pl.Data[:l+8]
			// Shift the payload to the right.
			copy(pl.Data[8:], pl.Data)
			// Write the CSTP header.
			copy(pl.Data[:8], proto.Header)
			// Update the payload length in the header.
			binary.BigEndian.PutUint16(pl.Data[4:6], uint16(l))
		} else {
			pl.Data = append(pl.Data[:0], proto.Header...)
			// Header-only control packet.
			pl.Data[6] = pl.Type
		}
		bytesSent, err = conn.Write(pl.Data)
		if err != nil {
			base.Error("tls payloadOut to server error:", err)
			return
		}
		cSess.Stat.BytesSent += uint64(bytesSent)

		// Return the buffer allocated by tunToPayloadOut.
		putPayloadBuffer(pl)
	}
}
