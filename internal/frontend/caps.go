package frontend

import (
	"errors"
	"fmt"
)

// CapabilityError marks a protocol's inability to express a capability.
// Per the frontend semantic rule: reject or degrade at the seam with a
// protocol-appropriate error — never silent emulation.
type CapabilityError struct {
	Capability string
}

func (e *CapabilityError) Error() string {
	return fmt.Sprintf("frontend does not support capability %q", e.Capability)
}

// ErrCapability returns a *CapabilityError for the named capability.
func ErrCapability(capability string) error {
	return &CapabilityError{Capability: capability}
}

// IsCapabilityError reports whether err (or any error it wraps) is a
// capability negotiation error.
func IsCapabilityError(err error) bool {
	var ce *CapabilityError
	return errors.As(err, &ce)
}
