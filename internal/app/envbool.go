package app

import (
	"strconv"
	"strings"
)

// envBool reads a boolean environment variable, returning the fallback when
// the variable is unset or unparsable. Values parse like strconv.ParseBool
// ("1", "t", "true", "yes", "on", "0", "f", "false", "no", "off") after
// trimming surrounding whitespace.
func envBool(getenv func(string) string, key string, fallback bool) bool {
	if getenv == nil {
		return fallback
	}

	rawValue := strings.TrimSpace(getenv(key))
	if rawValue == "" {
		return fallback
	}

	parsedValue, err := strconv.ParseBool(rawValue)
	if err != nil {
		return fallback
	}

	return parsedValue
}
