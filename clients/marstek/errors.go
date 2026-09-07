package marstek

import "errors"

// ErrLinkDown reports that the battery bridge is reachable over the network but
// the underlying RS485/Modbus link to the battery is not responding. Unless an
// error also wraps ErrControlNotAttempted, an individual command's outcome is
// unknown.
var ErrLinkDown = errors.New("battery RS485 link down")

// ErrControlNotAttempted reports that a charge or discharge request failed
// before its forcible-mode write was issued.
var ErrControlNotAttempted = errors.New("battery control not attempted")
