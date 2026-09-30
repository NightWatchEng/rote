// Command legacybank runs the MeridianCore simulator: the legacy back-office
// application the automation is pointed at.
package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"

	"github.com/NightWatchEng/rote/internal/legacybank"
)

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func main() {
	addr := flag.String("addr", "127.0.0.1:8080", "listen address")
	variant := flag.String("variant", "pinecrest", "tenant variant: pinecrest | lakeshore")
	flag.Parse()

	v, ok := legacybank.Variants[*variant]
	if !ok {
		fmt.Fprintf(os.Stderr, "unknown variant %q\n", *variant)
		os.Exit(2)
	}
	// Simulator-only credentials. They guard nothing real; the defaults exist
	// so the demo runs with no setup.
	srv := legacybank.New(legacybank.Config{
		Variant:          v,
		OperatorID:       env("LEGACYBANK_OPERATOR_ID", "TELLER01"),
		OperatorPassword: env("LEGACYBANK_OPERATOR_PASSWORD", "demo-teller-pass"),
		OverrideCode:     env("LEGACYBANK_OVERRIDE_CODE", "SUP-4471"),
	})
	log.Printf("MeridianCore simulator (%s, v%s) listening on http://%s", v.Institution, v.Version, *addr)
	log.Fatal(http.ListenAndServe(*addr, srv))
}
