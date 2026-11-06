package e2e

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/NodeSpy/conductor-plugins/internal/rpctest"
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
			if _, err := os.Stat(filepath.Join("..", kind, e.Name(), "main.go")); err != nil {
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
			})
		}
	}
}
