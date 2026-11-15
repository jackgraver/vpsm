package query

import "errors"

// ErrNoHost is returned by Source.Host when the requested id is not known to
// the collector.
var ErrNoHost = errors.New("no such host")
