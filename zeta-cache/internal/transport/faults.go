package transport

// faults.go - test-fault injection accessors for MemFS. The fields are
// unexported on purpose (leaf-02 ownership); tests outside the package
// drive them through these setters. GateErr wraps cause so tests can
// assert WHICH gate failed; engines map any gate error to their
// "on any doubt fall back" path (the token is a hint, never a
// correctness input).

import "fmt"

// GateError is the error tests inject into a specific verb gate. When
// OnlyKey is set, only that key's Put fails (surgical conflict tests).
type GateError struct {
	Gate    string // "propfind" | "put" | "get" | "delete"
	Cause   error
	OnlyKey *string // nil = every Put fails; else only this key's Put
}

func (e *GateError) Error() string {
	return fmt.Sprintf("transport: injected %s failure: %v", e.Gate, e.Cause)
}
func (e *GateError) Unwrap() error { return e.Cause }

// FailPropfinds makes every Propfind fail with a GateError until cleared.
func (m *MemFS) FailPropfinds() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.propfindErr = &GateError{Gate: "propfind", Cause: errInjected}
}

// FailPutsWith makes every Put fail with err until cleared (nil clears);
// pass &GateErr{Gate: "put", ...} to name the gate.
func (m *MemFS) FailPutsWith(err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.putErr = err
}

// ClearFaults clears every injected failure.
func (m *MemFS) ClearFaults() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.propfindErr, m.putErr, m.getErr, m.allErr = nil, nil, nil, nil
}

// errInjected is the default cause for injected gate failures.
var errInjected = fmt.Errorf("injected fault")
