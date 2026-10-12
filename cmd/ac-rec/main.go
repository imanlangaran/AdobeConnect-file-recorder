// ac-rec downloads and reconstructs Adobe Connect offline recordings.
package main

import (
	"os"

	"github.com/iman-langaran/AdobeConnect-file-recorder/internal/cli"
)

func main() {
	os.Exit(cli.Main(os.Args))
}
