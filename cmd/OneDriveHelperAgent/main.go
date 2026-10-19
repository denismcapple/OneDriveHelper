// OneDriveHelperAgent.exe runs in each user's session (GUI subsystem, no
// console window). It is started by the service, shows the alert popups and
// handles their buttons. "remediate-approve" is the UAC-elevated approval step.
package main

import (
	"context"
	"flag"
	"os"
	"time"

	"github.com/denismcapple/OneDriveHelper/internal/agent"
	"github.com/denismcapple/OneDriveHelper/internal/config"
	"github.com/denismcapple/OneDriveHelper/internal/logx"
	"github.com/denismcapple/OneDriveHelper/internal/remediate"
	"github.com/denismcapple/OneDriveHelper/internal/version"
	"github.com/denismcapple/OneDriveHelper/internal/winapi"
)

func main() {
	os.Exit(run())
}

func run() int {
	winapi.EnableVisualStyles()
	if len(os.Args) < 2 {
		winapi.Info(version.ProductName, "OneDrive Helper "+version.Version+
			"\n\nThis program is started automatically by the OneDrive Helper service.")
		return 2
	}
	switch os.Args[1] {
	case "agent":
		fs := flag.NewFlagSet("agent", flag.ContinueOnError)
		standalone := fs.Bool("standalone", false, "keep running even if the service is not running")
		stopEvent := fs.String("stop-event", "", "event signalled by the service when it stops")
		_ = fs.Parse(os.Args[2:])
		if err := agent.Run(context.Background(), *standalone, *stopEvent); err != nil {
			logx.Open(agent.LogPath(), false).Printf("agent: %v", err)
			return 1
		}
		return 0
	case "remediate-approve":
		fs := flag.NewFlagSet("remediate-approve", flag.ContinueOnError)
		sid := fs.String("sid", "", "user SID")
		deadline := fs.String("deadline", "", "Unix time after which nothing may be approved")
		start := time.Now()
		_ = fs.Parse(os.Args[2:])
		return remediate.RunApproval(*sid, remediate.ParseDeadline(*deadline, start))
	case "test-popup":
		if err := agent.RunSample(); err != nil {
			winapi.Error(config.Load().Title(), err.Error())
			return 1
		}
		return 0
	}
	return 2
}
