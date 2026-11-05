package runtime

import (
	"context"
	"fmt"
	"os"

	transporthttp "github.com/arksnorman/open-kase/api/internal/transport/http"
)

// runOpenAPI writes the generated OpenAPI 3.1 document. It needs no
// configuration or database: the document is built from the route table
// and DTO types compiled into the binary.
func runOpenAPI(_ context.Context, args []string, stdio IO) int {
	fs := newFlagSet("openapi", "Write the OpenAPI 3.1 document of the REST API (the same document served at /api/v1/openapi.json).")
	out := fs.String("out", "-", "output file (\"-\" writes to standard output)")
	if code, stop := parseFlags(fs, args, stdio); stop {
		return code
	}
	doc := transporthttp.OpenAPIDocument()
	if *out == "-" {
		if _, err := stdio.Out.Write(doc); err != nil {
			return ExitFailure
		}
		return ExitOK
	}
	if err := os.WriteFile(*out, doc, 0o644); err != nil {
		fmt.Fprintf(stdio.Err, "openapi: %v\n", err)
		return ExitFailure
	}
	fmt.Fprintf(stdio.Out, "wrote %s (%d bytes)\n", *out, len(doc))
	return ExitOK
}
