package config

import (
	"strings"
	"testing"
)

func resolveRebaseIntegrate(t *testing.T, globalYAML, trustedRepoYAML, pushedRepoYAML string) string {
	t.Helper()
	global, err := LoadGlobalFromBytes([]byte(globalYAML))
	if err != nil {
		t.Fatalf("load global config: %v", err)
	}
	trusted, err := LoadRepoFromBytes([]byte(trustedRepoYAML))
	if err != nil {
		t.Fatalf("load trusted repo config: %v", err)
	}
	pushed, err := LoadRepoFromBytes([]byte(pushedRepoYAML))
	if err != nil {
		t.Fatalf("load pushed repo config: %v", err)
	}
	return Merge(global, EffectiveRepoConfig(pushed, trusted, false)).Rebase.Integrate
}

// A head that merges cleanly into its base is kept unless a maintainer asks
// for every moved base to be integrated (issue #237).
func TestRebaseIntegrate_DefaultsToOnConflict(t *testing.T) {
	t.Parallel()
	const unset = unsetRebaseYAML
	if got := resolveRebaseIntegrate(t, unset, unset, unset); got != RebaseIntegrateOnConflict {
		t.Fatalf("Rebase.Integrate = %q, want %q for an unset config", got, RebaseIntegrateOnConflict)
	}
	if got := resolveRebaseIntegrate(t, "rebase:\n  integrate: \"\"\n", unset, unset); got != RebaseIntegrateOnConflict {
		t.Fatalf("Rebase.Integrate = %q, want %q for an empty value", got, RebaseIntegrateOnConflict)
	}
}

func TestRebaseIntegrate_GlobalAndTrustedRepoPrecedence(t *testing.T) {
	t.Parallel()
	const unset = unsetRebaseYAML
	const always = "rebase:\n  integrate: always\n"
	const onConflict = "rebase:\n  integrate: on-conflict\n"
	if got := resolveRebaseIntegrate(t, always, unset, unset); got != RebaseIntegrateAlways {
		t.Fatalf("global always = %q", got)
	}
	if got := resolveRebaseIntegrate(t, always, onConflict, unset); got != RebaseIntegrateOnConflict {
		t.Fatalf("the trusted repository value must win over global: %q", got)
	}
}

// Whether a moved base invalidates a branch's evidence is gate control: a
// pushed branch must not change it in either direction.
func TestRebaseIntegrate_TrustedOnly(t *testing.T) {
	t.Parallel()
	const unset = unsetRebaseYAML
	const always = "rebase:\n  integrate: always\n"
	const onConflict = "rebase:\n  integrate: on-conflict\n"
	if got := resolveRebaseIntegrate(t, unset, always, onConflict); got != RebaseIntegrateAlways {
		t.Fatalf("Rebase.Integrate = %q: a pushed branch must not relax the trusted value", got)
	}
	if got := resolveRebaseIntegrate(t, unset, unset, always); got != RebaseIntegrateOnConflict {
		t.Fatalf("Rebase.Integrate = %q: a pushed branch must not select the value on its own", got)
	}
}

func TestRebaseIntegrate_RejectsUnknownValue(t *testing.T) {
	t.Parallel()
	for _, load := range []func([]byte) error{
		func(b []byte) error { _, err := LoadRepoFromBytes(b); return err },
		func(b []byte) error { _, err := LoadGlobalFromBytes(b); return err },
	} {
		err := load([]byte("rebase:\n  integrate: sometimes\n"))
		if err == nil || !strings.Contains(err.Error(), "rebase.integrate") {
			t.Fatalf("an unknown rebase.integrate must fail closed and name the setting: %v", err)
		}
	}
}
