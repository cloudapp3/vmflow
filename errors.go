package vmflow

import "errors"

// ErrRuntimeClosed is returned when callers try to mutate a closed Runtime.
var ErrRuntimeClosed = errors.New("vmflow runtime closed")
