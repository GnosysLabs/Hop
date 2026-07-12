package hop

import (
	"context"
	"testing"
)

func TestConfigureRemoteRequiresExplicitReplacement(t *testing.T) {
	root := t.TempDir()
	runGitTest(t, root, "init", "--quiet")
	repository, err := OpenRepository(root)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	first := "https://forge.example/alice/first.git"
	second := "https://forge.example/alice/second.git"
	if err := repository.PreflightRemote(ctx, "origin", false); err != nil {
		t.Fatal(err)
	}
	if err := repository.ConfigureRemote(ctx, "origin", first, false); err != nil {
		t.Fatal(err)
	}
	if err := repository.PreflightRemote(ctx, "origin", false); err == nil {
		t.Fatal("existing remote passed preflight without explicit replacement")
	}
	if err := repository.ConfigureRemote(ctx, "origin", second, true); err != nil {
		t.Fatal(err)
	}
	if got := runGitTest(t, root, "remote", "get-url", "origin"); got != second {
		t.Fatalf("remote URL = %q, want %q", got, second)
	}
	if err := repository.ConfigureRemote(ctx, "../bad", first, false); err == nil {
		t.Fatal("unsafe remote name was accepted")
	}
}
