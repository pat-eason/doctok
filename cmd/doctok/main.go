// Command doctok turns technical docs into TikTok-influencer explainer videos.
package main

import (
	"os"

	"github.com/pat-eason/doctok/internal/cli"
)

var version = "dev"

func main() {
	cli.Version = version
	os.Exit(cli.Execute())
}
