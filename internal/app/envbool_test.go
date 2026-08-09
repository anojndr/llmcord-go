package app

import (
	"testing"
)

func TestEnvBoolParsesTruthyValues(t *testing.T) {
	t.Parallel()

	for _, rawValue := range []string{"1", "t", "T", "true", "TRUE", "True"} {
		parsed := envBool(func(string) string { return rawValue }, "TEST_ENV_BOOL", false)
		if !parsed {
			t.Fatalf("envBool(%q) = false, want true", rawValue)
		}
	}
}

func TestEnvBoolParsesFalsyValues(t *testing.T) {
	t.Parallel()

	for _, rawValue := range []string{"0", "f", "F", "false", "FALSE", "False"} {
		parsed := envBool(func(string) string { return rawValue }, "TEST_ENV_BOOL", true)
		if parsed {
			t.Fatalf("envBool(%q) = true, want false", rawValue)
		}
	}
}

func TestEnvBoolFallsBackForUnsetOrInvalid(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name     string
		rawValue string
		fallback bool
	}{
		{name: "unset", rawValue: "", fallback: true},
		{name: "whitespace", rawValue: "   ", fallback: false},
		{name: "garbage", rawValue: "definitely-not-a-bool", fallback: true},
		{name: "empty fallback", rawValue: "", fallback: false},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			parsed := envBool(func(string) string { return testCase.rawValue }, "TEST_ENV_BOOL", testCase.fallback)
			if parsed != testCase.fallback {
				t.Fatalf(
					"envBool(%q) = %t, want fallback %t",
					testCase.rawValue,
					parsed,
					testCase.fallback,
				)
			}
		})
	}
}

func TestEnvBoolFallsBackForNilGetter(t *testing.T) {
	t.Parallel()

	if parsed := envBool(nil, "TEST_ENV_BOOL", false); parsed {
		t.Fatal("envBool(nil, ...) = true, want false")
	}
}
