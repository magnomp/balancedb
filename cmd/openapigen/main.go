// Command openapigen exports BalanceDB's code-first OpenAPI 3.1 document to a
// file (default api/openapi.yaml). The server is the authority (ADR-0003): this
// tool builds the same Huma API the process serves and writes its generated spec,
// so `make openapi` produces the committed contract artifact. It touches no
// database — the operations and their schemas are fixed at registration.
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/magnomp/balancedb/internal/api"
)

func main() {
	out := flag.String("o", "api/openapi.yaml", "output path for the generated OpenAPI YAML")
	flag.Parse()

	yaml, err := api.OpenAPIYAML()
	if err != nil {
		fmt.Fprintln(os.Stderr, "openapigen:", err)
		os.Exit(1)
	}
	if err := os.WriteFile(*out, yaml, 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "openapigen: write", *out+":", err)
		os.Exit(1)
	}
	fmt.Fprintln(os.Stderr, "openapigen: wrote", *out)
}
