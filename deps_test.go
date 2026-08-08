package lightsql

import (
	"os"
	"strings"
	"testing"
)

// allowedPrefixes lists the module paths the root module may depend on at run
// time.
var allowedPrefixes = []string{"golang.org/x/"}

// TestNoRuntimeDependencies enforces the promise the README makes: importing
// lightsql must not pull a dependency tree into the importing project.
//
// This is checked mechanically rather than by review because the cost of a new
// dependency is paid by every downstream user, and a require line is easy to add
// without noticing. Test-only dependencies belong in the separate compat/
// module, which is not covered by this check.
func TestNoRuntimeDependencies(t *testing.T) {
	data, err := os.ReadFile("go.mod")
	if err != nil {
		t.Fatalf("reading go.mod: %v", err)
	}

	for _, mod := range requiredModules(string(data)) {
		if !allowed(mod) {
			t.Errorf("go.mod requires %q.\n"+
				"The root module may only depend on the standard library and %s.\n"+
				"Put test-only dependencies in the compat/ module instead.",
				mod, strings.Join(allowedPrefixes, ", "))
		}
	}
}

func allowed(mod string) bool {
	for _, p := range allowedPrefixes {
		if strings.HasPrefix(mod, p) {
			return true
		}
	}
	return false
}

// requiredModules extracts module paths from both the block and single-line
// forms of a require directive.
func requiredModules(gomod string) []string {
	var mods []string
	inBlock := false

	for line := range strings.Lines(gomod) {
		line = strings.TrimSpace(line)
		if i := strings.Index(line, "//"); i >= 0 {
			line = strings.TrimSpace(line[:i])
		}
		switch {
		case line == "require (":
			inBlock = true
		case inBlock && line == ")":
			inBlock = false
		case inBlock && line != "":
			mods = append(mods, strings.Fields(line)[0])
		case strings.HasPrefix(line, "require "):
			if f := strings.Fields(line); len(f) >= 2 {
				mods = append(mods, f[1])
			}
		}
	}
	return mods
}
