package mate

import (
	"context"
)

func BaseContext(platformInterface PlatformInterface) context.Context {
	ctx := context.Background()
	return ctx
}
