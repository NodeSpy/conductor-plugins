package e2e

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/NodeSpy/conductor-plugins/internal/rpctest"
	"github.com/NodeSpy/conductor/pkg/plugin"
)

// Conductor jails every plugin to exactly what it declares (Capabilities)
// and refuses a declaration that does not validate: an entry that does not
// parse, a config: reference to a connection field the plugin does not
// declare, an env: reference to a variable not in Env. Every plugin in this
// repo must describe a declaration conductor accepts, over the real
// transport.
func TestEveryDeclarationValidates(t *testing.T) {
	for _, kind := range []string{"connectors", "runtimes", "engines"} {
		ents, err := os.ReadDir(filepath.Join("..", kind))
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range ents {
			if !e.IsDir() {
				continue
			}
			if !isMainPackage(t, filepath.Join("..", kind, e.Name())) {
				continue
			}
			kind, name := kind, e.Name()
			t.Run(kind+"/"+name, func(t *testing.T) {
				t.Parallel()
				c := rpctest.Start(t, rpctest.Build(t, kind, name))
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				decl, err := c.Describe(ctx)
				if err != nil {
					t.Fatal(err)
				}
				if err := decl.Capabilities.Validate(decl.Connection); err != nil {
					t.Fatalf("declaration refused: %v", err)
				}
				// A conductor that does not resolve config: and env: entries
				// gets the declaration without them.
				old, err := c.DescribeAs(ctx, &plugin.HostInfo{Version: "dev"})
				if err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(old.Capabilities, decl.Capabilities.WithoutReferences()) {
					t.Fatalf("declaration for a conductor that does not resolve references = %+v, want %+v", old.Capabilities, decl.Capabilities.WithoutReferences())
				}
			})
		}
	}
}

// isMainPackage reports whether dir holds a plugin binary's package: its
// func main need not be in main.go.
func isMainPackage(t *testing.T, dir string) bool {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.HasPrefix(b, []byte("package main\n")) || bytes.Contains(b, []byte("\npackage main\n")) {
			return true
		}
	}
	return false
}
