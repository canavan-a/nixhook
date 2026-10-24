// Package flake builds flake references for the configured source, which is
// either a local git checkout or any flake URI nix understands.
package flake

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/canavan-a/nixhook/internal/system"
)

var (
	targetRe = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)
	refRe    = regexp.MustCompile(`^[A-Za-z0-9._/-]+$`)
)

// ValidTarget reports whether name is safe to use as a nixosConfigurations
// attribute.
func ValidTarget(name string) bool { return targetRe.MatchString(name) }

// ValidRef reports whether ref is safe to pass to git and nix as a branch or
// tag name.
func ValidRef(ref string) bool { return refRe.MatchString(ref) && !strings.HasPrefix(ref, "-") }

// Source is where the system configuration comes from.
type Source string

// Local reports whether the source is a checkout on disk, which is synced
// with git pull. Anything else is treated as a flake URI and fetched by nix.
func (s Source) Local() bool {
	return strings.HasPrefix(string(s), "/") || strings.HasPrefix(string(s), ".")
}

// URI returns the flake reference for ref. Local checkouts are used as they
// are on disk; remote URIs get ref as a query parameter.
func (s Source) URI(ref string) string { return s.with("ref", ref) }

// Pinned returns the flake reference locked to rev, so every step of a deploy
// builds the same commit even if the branch moves.
func (s Source) Pinned(rev string) string { return s.with("rev", rev) }

func (s Source) with(key, value string) string {
	if s.Local() || value == "" {
		return string(s)
	}
	sep := "?"
	if strings.Contains(string(s), "?") {
		sep = "&"
	}
	return string(s) + sep + key + "=" + value
}

// Config is the reference nixos-rebuild --flake takes for target.
func Config(uri, target string) string {
	return uri + "#" + target
}

// Toplevel is the installable for target's system closure.
func Toplevel(uri, target string) string {
	return uri + "#nixosConfigurations." + target + ".config.system.build.toplevel"
}

// Targets lists the nixosConfigurations names defined by uri, one of s.URI or
// s.Pinned. Remote sources are refetched so newly added hosts show up.
func (s Source) Targets(ctx context.Context, uri string) ([]string, error) {
	args := append([]string{"eval", "--json"}, s.refreshArgs()...)
	args = append(args, uri+"#nixosConfigurations", "--apply", "builtins.attrNames")
	out, err := system.Output(ctx, "nix", args...)
	if err != nil {
		return nil, err
	}
	var names []string
	if err := json.Unmarshal([]byte(out), &names); err != nil {
		return nil, fmt.Errorf("parse nixosConfigurations: %w", err)
	}
	slices.Sort(names)
	return names, nil
}

// Rev returns the commit ref currently points at. For a local checkout that is
// HEAD, whatever ref is.
func (s Source) Rev(ctx context.Context, ref string) (string, error) {
	if s.Local() {
		return system.Output(ctx, "git", s.GitArgs("rev-parse", "HEAD")...)
	}
	args := append([]string{"flake", "metadata", "--json"}, s.refreshArgs()...)
	out, err := system.Output(ctx, "nix", append(args, s.URI(ref))...)
	if err != nil {
		return "", err
	}
	var meta struct {
		Revision string `json:"revision"`
		Locked   struct {
			Rev string `json:"rev"`
		} `json:"locked"`
	}
	if err := json.Unmarshal([]byte(out), &meta); err != nil {
		return "", fmt.Errorf("parse flake metadata: %w", err)
	}
	if meta.Revision != "" {
		return meta.Revision, nil
	}
	if meta.Locked.Rev != "" {
		return meta.Locked.Rev, nil
	}
	return "", fmt.Errorf("flake metadata for %s has no revision", s.URI(ref))
}

// GitArgs prefixes args so git operates on the local checkout regardless of
// who owns it.
func (s Source) GitArgs(args ...string) []string {
	return append([]string{"-c", "safe.directory=" + string(s), "-C", string(s)}, args...)
}

// refreshArgs make nix fetch a remote source again instead of using a cached
// copy of the branch.
func (s Source) refreshArgs() []string {
	if s.Local() {
		return nil
	}
	return []string{"--refresh"}
}
