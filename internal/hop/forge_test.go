package hop

import (
	"reflect"
	"testing"
)

func TestForgeKindForHost(t *testing.T) {
	for _, test := range []struct {
		host, override string
		want           ForgeKind
	}{
		{"github.com", "", ForgeGitHub},
		{"gitlab.com", "", ForgeGitLab},
		{"githop.xyz", "", ForgeGitea},
		{"git.example.com", "", ForgeGeneric},
		{"git.example.com", "gitea", ForgeGitea},
	} {
		if got := forgeKindForHost(test.host, test.override); got != test.want {
			t.Fatalf("forgeKindForHost(%q, %q) = %q, want %q", test.host, test.override, got, test.want)
		}
	}
}

func TestGitHubCommandTranslation(t *testing.T) {
	for _, test := range []struct {
		input, want []string
	}{
		{[]string{"issues", "list"}, []string{"issue", "list"}},
		{[]string{"pulls", "create", "--fill"}, []string{"pr", "create", "--fill"}},
		{[]string{"releases", "view", "v1.1.0"}, []string{"release", "view", "v1.1.0"}},
		{[]string{"whoami"}, []string{"api", "user", "--jq", ".login"}},
	} {
		got, ok := githubCommand(test.input)
		if !ok || !reflect.DeepEqual(got, test.want) {
			t.Fatalf("githubCommand(%v) = %v, %v; want %v, true", test.input, got, ok, test.want)
		}
	}
}

func TestGitLabCommandTranslation(t *testing.T) {
	got, ok := gitlabCommand([]string{"pulls", "list"})
	if !ok || !reflect.DeepEqual(got, []string{"mr", "list"}) {
		t.Fatalf("gitlab pulls translation = %v, %v", got, ok)
	}
}
