package flake

import "testing"

func TestSource(t *testing.T) {
	for _, tc := range []struct {
		src, ref, uri, pinned string
		local                 bool
	}{
		{"/etc/nixos", "main", "/etc/nixos", "/etc/nixos", true},
		{"./sys", "main", "./sys", "./sys", true},
		{"github:o/r", "main", "github:o/r?ref=main", "github:o/r?rev=abc", false},
		{"git+https://x/r?dir=sys", "v1", "git+https://x/r?dir=sys&ref=v1", "git+https://x/r?dir=sys&rev=abc", false},
		{"github:o/r", "", "github:o/r", "github:o/r?rev=abc", false},
	} {
		s := Source(tc.src)
		if s.Local() != tc.local {
			t.Errorf("%s: Local() = %v", tc.src, s.Local())
		}
		if got := s.URI(tc.ref); got != tc.uri {
			t.Errorf("%s: URI(%q) = %q, want %q", tc.src, tc.ref, got, tc.uri)
		}
		if got := s.Pinned("abc"); got != tc.pinned {
			t.Errorf("%s: Pinned = %q, want %q", tc.src, got, tc.pinned)
		}
	}

	if got := Config("github:o/r?rev=abc", "box"); got != "github:o/r?rev=abc#box" {
		t.Errorf("Config = %q", got)
	}
	if got := Toplevel("/etc/nixos", "box"); got != "/etc/nixos#nixosConfigurations.box.config.system.build.toplevel" {
		t.Errorf("Toplevel = %q", got)
	}
}

func TestValidation(t *testing.T) {
	for _, name := range []string{"homeServer", "web-1", "a_b"} {
		if !ValidTarget(name) {
			t.Errorf("ValidTarget(%q) = false", name)
		}
	}
	for _, name := range []string{"", "a.b", "a b", "x;rm", `a"b`} {
		if ValidTarget(name) {
			t.Errorf("ValidTarget(%q) = true", name)
		}
	}
	for _, ref := range []string{"main", "release/26.05", "v1.2.3"} {
		if !ValidRef(ref) {
			t.Errorf("ValidRef(%q) = false", ref)
		}
	}
	for _, ref := range []string{"", "-u", "--upload-pack=x", "a b", "a&b"} {
		if ValidRef(ref) {
			t.Errorf("ValidRef(%q) = true", ref)
		}
	}
}
