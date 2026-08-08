package providers

import "errors"

// ErrEmptyModelResponse is returned when a provider streams to a clean end
// without producing any content.
var ErrEmptyModelResponse = errors.New("model returned an empty response")
