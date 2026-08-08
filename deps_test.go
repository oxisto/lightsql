package lightsql

import (
	"encoding/json"
	"os/exec"
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
	// `go mod edit -json` is the toolchain's own view of the file. Parsing
	// go.mod by hand would mean reimplementing a format that has more forms
	// than it first appears to, and getting it subtly wrong would weaken the
	// check rather than fail it.
	out, err := exec.Command("go", "mod", "edit", "-json").Output()
	if err != nil {
		t.Fatalf("go mod edit -json: %v", err)
	}

	var mod struct {
		Require []struct {
			Path     string
			Indirect bool
		}
	}
	if err := json.Unmarshal(out, &mod); err != nil {
		t.Fatalf("parsing go.mod: %v", err)
	}

	for _, req := range mod.Require {
		if allowed(req.Path) {
			continue
		}
		kind := "a direct dependency"
		if req.Indirect {
			kind = "an indirect dependency"
		}
		t.Errorf("go.mod requires %q as %s.\n"+
			"The root module may only depend on the standard library and %s.\n"+
			"Put test-only dependencies in the compat/ module instead.",
			req.Path, kind, strings.Join(allowedPrefixes, ", "))
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
