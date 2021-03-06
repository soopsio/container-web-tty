package server

import (
	"github.com/wrfly/container-web-tty/gotty/webtty"
)

// Slave is webtty.Slave with some additional methods.
type Slave interface {
	webtty.Slave

	Close() error
}

type Factory interface {
	Name() string
	New(args []string) (Slave, error)
}
