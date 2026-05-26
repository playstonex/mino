package vpn

import (
	"sync"

	"github.com/WarrDoge/sslcon/proto"
)

const BufferSize = 2048

// pool stores the actual payload buffers; Go manages the backing capacity, and channels such as PayloadIn only carry pointers.
var pool = sync.Pool{
	New: func() interface{} {
		b := make([]byte, BufferSize)
		pl := proto.Payload{
			Type: 0x00,
			Data: b,
		}
		return &pl
	},
}

func getPayloadBuffer() *proto.Payload {
	pl := pool.Get().(*proto.Payload)
	return pl
}

func putPayloadBuffer(pl *proto.Payload) {
	// Control packets such as DPD-REQ and KEEPALIVE.
	if cap(pl.Data) != BufferSize {
		// base.Debug("payload is:", pl.Data)
		return
	}

	pl.Type = 0x00
	pl.Data = pl.Data[:BufferSize]
	pool.Put(pl)
}
