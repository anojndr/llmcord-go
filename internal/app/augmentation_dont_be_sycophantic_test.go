package app

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestContainsDontBeSycophanticPhrase(t *testing.T) {
	t.Parallel()

	cases := map[string]bool{
		"don't be sycophantic":  true,
		"Don't Be Sycophantic.": true,
		"dont be sycophantic":   true,
		"please explain this":   false,
	}

	for input, want := range cases {
		if got := containsDontBeSycophanticPhrase(input); got != want {
			t.Errorf("containsDontBeSycophanticPhrase(%q) = %v, want %v", input, got, want)
		}
	}
}

func TestApplyAutoAppendDontBeSycophantic(t *testing.T) {
	t.Parallel()

	conversation := []chatMessage{
		{Role: messageRoleUser, Content: "explain this"},
	}

	appended, err := applyAutoAppend(providerConfig{AutoAppendDontBeSycophantic: true}, conversation)
	if err != nil {
		t.Fatalf("apply auto append: %v", err)
	}

	query, ok := latestUserQuery(appended)
	if !ok {
		t.Fatal("expected user query in appended conversation")
	}

	if !strings.Contains(query, dontBeSycophanticPhrase) {
		t.Fatalf("expected %q in query, got %q", dontBeSycophanticPhrase, query)
	}

	if dontBeSycophanticPhrase != "don't be sycophantic." {
		t.Fatalf("unexpected phrase %q", dontBeSycophanticPhrase)
	}
}

func TestAutoAppendDontBeSycophanticConfigKey(t *testing.T) {
	t.Parallel()

	var raw rawProviderConfig
	if err := yaml.Unmarshal([]byte("auto_append_dont_be_sycophantic: true\n"), &raw); err != nil {
		t.Fatalf("unmarshal provider config: %v", err)
	}

	if raw.AutoAppendDontBeSycophantic == nil || !*raw.AutoAppendDontBeSycophantic {
		t.Fatal("expected auto_append_dont_be_sycophantic to parse as true")
	}
}
