package metadata

// PathStatError reports a failed statfs during ZFS detection.
type PathStatError struct {
	Path string
	Err  error
}

func (e *PathStatError) Error() string {
	return "metadata: statfs " + e.Path + ": " + e.Err.Error()
}
func (e *PathStatError) Unwrap() error { return e.Err }
