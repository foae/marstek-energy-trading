package marstek

import "errors"

// ErrLinkDown reports that the battery bridge is reachable over the network but
// the underlying RS485/Modbus link to the battery is not responding: telemetry
// is frozen at stale values and control writes are silently dropped.
var ErrLinkDown = errors.New("battery RS485 link down")
