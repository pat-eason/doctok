package cli

import (
	"fmt"
	"net"
	"strconv"

	"github.com/pat-eason/doctok/engine"
	"github.com/pat-eason/doctok/internal/ui"
	"github.com/spf13/cobra"
)

func newUICmd() *cobra.Command {
	var port int
	var noOpen bool
	cmd := &cobra.Command{
		Use:   "ui",
		Short: "Launch the local web UI (localhost only)",
		RunE: func(cmd *cobra.Command, args []string) error {
			home, err := engine.HomeDir()
			if err != nil {
				return err
			}
			addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
			return ui.Serve(cmd.Context(), home, addr, func(url string) {
				fmt.Println(logo())
				fmt.Println(sOK.Render("✓ studio running at ") + sBold.Render(url) + sDim.Render("  (ctrl+c to stop; running jobs pause and stay resumable)"))
				if !noOpen {
					_ = openPath(url)
				}
			})
		},
	}
	cmd.Flags().IntVarP(&port, "port", "p", 7878, "port to listen on (127.0.0.1 only)")
	cmd.Flags().BoolVar(&noOpen, "no-open", false, "don't open the browser")
	return cmd
}
