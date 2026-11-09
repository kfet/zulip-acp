// Command schemagen writes internal/config/schema_gen.go. Run it with
// `go generate ./internal/config`.
package main

import (
	"log"
	"os"

	"github.com/kfet/zulip-acp/internal/config/schemadoc"
)

func main() {
	src, err := schemadoc.Generate(".")
	if err != nil {
		log.Fatal(err)
	}
	if err := os.WriteFile("schema_gen.go", src, 0o644); err != nil {
		log.Fatal(err)
	}
}
