package peers

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSanitizeName(t *testing.T) {
	cases := map[string]string{
		"pith":             "pith",
		"My App":           "my-app",
		"foo.bar":          "foo-bar", // '.' would break peers.msg.<name> subjects
		"weird*chars>here": "weird-chars-here",
		"  spaced  ":       "spaced",
		"---":              "",
		"UPPER_case-9":     "upper_case-9",
		"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", // capped at 32
	}
	for in, want := range cases {
		if got := SanitizeName(in); got != want {
			t.Errorf("SanitizeName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestResolveIdentity(t *testing.T) {
	dir := t.TempDir()
	proj := filepath.Join(dir, "My Cool.Project")
	if err := os.Mkdir(proj, 0o755); err != nil {
		t.Fatal(err)
	}

	// default: sanitized dir basename
	t.Setenv("CLAUDE_PEERS_AGENT", "")
	name, source := ResolveIdentity(proj, "")
	if name != "my-cool-project" || source != "default" {
		t.Errorf("default: got %q/%q", name, source)
	}

	// legacy marker file beats default — read forever, not for a deprecation
	// window: the fleet has ~20 of these already, and a cutover would
	// silently rename every dir whose marker differs from its basename back
	// to the dir default, the exact class of incident this file prevents.
	if err := os.WriteFile(filepath.Join(proj, ".claude-peers-agent"), []byte("filed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if name, source = ResolveIdentity(proj, ""); name != "filed" || source != "file" {
		t.Errorf("legacy file: got %q/%q", name, source)
	}

	// env beats file
	t.Setenv("CLAUDE_PEERS_AGENT", "envy")
	if name, source = ResolveIdentity(proj, ""); name != "envy" || source != "env" {
		t.Errorf("env: got %q/%q", name, source)
	}

	// explicit beats everything
	if name, source = ResolveIdentity(proj, "Chosen One"); name != "chosen-one" || source != "flag" {
		t.Errorf("flag: got %q/%q", name, source)
	}
}

func TestResolveIdentityMarkerMigration(t *testing.T) {
	t.Setenv("CLAUDE_PEERS_AGENT", "")
	dir := t.TempDir()

	t.Run("new canonical name alone", func(t *testing.T) {
		d := filepath.Join(dir, "new-only")
		os.Mkdir(d, 0o755)
		os.WriteFile(filepath.Join(d, ".cp3-peer-name"), []byte("newname\n"), 0o644)
		if name, source := ResolveIdentity(d, ""); name != "newname" || source != "file" {
			t.Errorf("got %q/%q, want newname/file", name, source)
		}
	})

	t.Run("both present: new name wins, legacy never consulted", func(t *testing.T) {
		d := filepath.Join(dir, "both")
		os.Mkdir(d, 0o755)
		os.WriteFile(filepath.Join(d, ".cp3-peer-name"), []byte("current\n"), 0o644)
		os.WriteFile(filepath.Join(d, ".claude-peers-agent"), []byte("stale\n"), 0o644)
		if name, _ := ResolveIdentity(d, ""); name != "current" {
			t.Errorf("got %q, want current (the new marker, not the stale legacy one)", name)
		}
	})
}
