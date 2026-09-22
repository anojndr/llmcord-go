package app

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestContainsCrossCheckPhrase(t *testing.T) {
	t.Parallel()

	cases := map[string]bool{
		"always cross-check to ensure accuracy":  true,
		"Always Cross-Check To Ensure Accuracy.": true,
		"please cross check this":                true,
		"crosscheck everything":                  true,
		"please explain this":                    false,
	}

	for input, want := range cases {
		if got := containsCrossCheckPhrase(input); got != want {
			t.Errorf("containsCrossCheckPhrase(%q) = %v, want %v", input, got, want)
		}
	}
}

func TestApplyAutoAppendCrossCheck(t *testing.T) {
	t.Parallel()

	conversation := []chatMessage{
		{Role: messageRoleUser, Content: "explain this"},
	}

	appended, err := applyAutoAppend(providerConfig{AutoAppendCrossCheck: true}, conversation)
	if err != nil {
		t.Fatalf("apply auto append: %v", err)
	}

	query, ok := latestUserQuery(appended)
	if !ok {
		t.Fatal("expected user query in appended conversation")
	}

	if !strings.Contains(query, crossCheckPhrase) {
		t.Fatalf("expected %q in query, got %q", crossCheckPhrase, query)
	}

	if crossCheckPhrase != "always cross-check to ensure accuracy." {
		t.Fatalf("unexpected phrase %q", crossCheckPhrase)
	}
}

func TestApplyAutoAppendCrossCheckSkipsWhenPresent(t *testing.T) {
	t.Parallel()

	conversation := []chatMessage{
		{Role: messageRoleUser, Content: "explain this. Always cross-check to ensure accuracy."},
	}

	appended, err := applyAutoAppend(providerConfig{AutoAppendCrossCheck: true}, conversation)
	if err != nil {
		t.Fatalf("apply auto append: %v", err)
	}

	query, ok := latestUserQuery(appended)
	if !ok {
		t.Fatal("expected user query in appended conversation")
	}

	if got := strings.Count(strings.ToLower(query), "cross-check"); got != 1 {
		t.Fatalf("expected single cross-check phrase, got %q", query)
	}
}
func TestAutoAppendCrossCheckConfigKey(t *testing.T) {
	t.Parallel()

	var raw rawProviderConfig
	if err := yaml.Unmarshal([]byte("auto_append_cross_check: true\n"), &raw); err != nil {
		t.Fatalf("unmarshal provider config: %v", err)
	}

	if raw.AutoAppendCrossCheck == nil || !*raw.AutoAppendCrossCheck {
		t.Fatal("expected auto_append_cross_check to parse as true")
	}
}
