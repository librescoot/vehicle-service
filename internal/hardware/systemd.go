package hardware

import (
	"fmt"
	"os"
	"strconv"
	"sync"

	"github.com/godbus/dbus/v5"
)

// unitSwitcher starts and stops systemd units. See systemdClient.
type unitSwitcher interface {
	SetUnitActive(unit string, active bool) error
	Close()
}

// The DBC link and the PPP backup are switched by unit, so the units own what
// the link means: librescoot-dbc-link.service is the DBC link's admin state,
// and the route monitor rides on it.
const (
	DbcLinkUnit = "librescoot-dbc-link.service"
	PppLinkUnit = "ppp-link.service"
)

const (
	systemdDestination = "org.freedesktop.systemd1"
	systemdPath        = dbus.ObjectPath("/org/freedesktop/systemd1")
	systemdManager     = "org.freedesktop.systemd1.Manager"
)

// systemdClient talks to systemd over the system bus.
//
// The bus is used instead of shelling out to systemctl: a state transition
// must not fork a dynamically linked binary, the same reason DbcLinks reads
// /proc/net/route rather than running ip. StartUnit returns once the job is
// queued, which is what systemctl --no-block did, and failures come back as
// values instead of an exit status and stderr to parse.
type systemdClient struct {
	mu   sync.Mutex
	conn *dbus.Conn
}

func (c *systemdClient) SetUnitActive(unit string, active bool) error {
	method := "StartUnit"
	if !active {
		method = "StopUnit"
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	conn, err := c.connection()
	if err != nil {
		return fmt.Errorf("systemd bus: %w", err)
	}

	var job dbus.ObjectPath
	if err := conn.Object(systemdDestination, systemdPath).
		Call(systemdManager+"."+method, 0, unit, "replace").Store(&job); err != nil {
		// Drop the connection so the next call reconnects: the bus can be
		// restarted underneath us, and every caller here is a state
		// transition that will come around again.
		c.close()
		return fmt.Errorf("systemd %s %s: %w", method, unit, err)
	}

	return nil
}

func (c *systemdClient) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.close()
}

// connection returns the cached bus connection, dialing it on first use so
// that a bus that is not up yet does not fail vehicle-service startup.
func (c *systemdClient) connection() (*dbus.Conn, error) {
	if c.conn != nil {
		return c.conn, nil
	}

	conn, err := dbus.SystemBusPrivate()
	if err != nil {
		return nil, err
	}
	if err := conn.Auth([]dbus.Auth{dbus.AuthExternal(strconv.Itoa(os.Getuid()))}); err != nil {
		conn.Close()
		return nil, fmt.Errorf("authenticate: %w", err)
	}
	if err := conn.Hello(); err != nil {
		conn.Close()
		return nil, fmt.Errorf("hello: %w", err)
	}

	c.conn = conn
	return conn, nil
}

func (c *systemdClient) close() {
	if c.conn != nil {
		c.conn.Close()
		c.conn = nil
	}
}
