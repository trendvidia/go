// Command exporttab is the host half of TestExportTableCompleteness:
// it opens every plugin named on the command line and reports success
// per argument. On Windows this exercises the loader-level half of the
// export contract — plugin.Open resolves every ptabs symbol through
// GetProcAddress, so an export missing from the PE export directory
// fails right here. The test itself adds the static half by parsing
// each image's export table.
package main

import (
	"fmt"
	"os"
	"plugin"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintf(os.Stderr, "usage: exporttab <plugin>...\n")
		os.Exit(2)
	}
	for _, arg := range os.Args[1:] {
		if _, err := plugin.Open(arg); err != nil {
			fmt.Fprintf(os.Stderr, "exporttab: Open(%q): %v\n", arg, err)
			os.Exit(1)
		}
		fmt.Printf("OPENED %s\n", arg)
	}
}
