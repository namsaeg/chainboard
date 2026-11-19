//go:build !linux

package nat

import "net"

// defaultGateway is not read on this system; the x.y.z.1 guesses remain.
func defaultGateway() net.IP { return nil }
