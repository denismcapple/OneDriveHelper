// OneDriveHelperConfig.exe edits OneDrive Helper's settings: a window when run
// without arguments, or --import / --export / --validate from the command line.
package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/denismcapple/OneDriveHelper/internal/config"
	"github.com/denismcapple/OneDriveHelper/internal/configui"
	"github.com/denismcapple/OneDriveHelper/internal/dialog"
	"github.com/denismcapple/OneDriveHelper/internal/version"
	"github.com/denismcapple/OneDriveHelper/internal/winapi"
)

const usage = `OneDrive Helper settings tool %s

  OneDriveHelperConfig.exe                     Open the settings window
  OneDriveHelperConfig.exe --open <file>       Open the window with a JSON settings file loaded
  OneDriveHelperConfig.exe --export <file>     Save this PC's settings to a JSON file
  OneDriveHelperConfig.exe --import <file>     Validate a JSON file and apply it to this PC (admin)
  OneDriveHelperConfig.exe --validate <file>   Check a JSON file without applying it

Deploy a settings file with: OneDriveHelper.exe install --config <file>

Exit codes: 0 success, 1 error, 2 usage.
`

func main() {
	if len(os.Args) < 2 || (len(os.Args) == 3 && strings.EqualFold(strings.TrimLeft(os.Args[1], "-/"), "open")) {
		file := ""
		if len(os.Args) == 3 {
			file = os.Args[2]
		}
		if err := configui.Run(file); err != nil {
			msg.Error("The settings window could not be opened", err.Error())
			os.Exit(1)
		}
		return
	}
	console := winapi.AttachParentConsole()
	code, text := run(os.Args[1:])
	switch {
	case text == "":
	case console:
		fmt.Println(text)
	case code != 0: // no console (e.g. started elevated by the settings window)
		msg.Error("The settings were not saved", text)
	}
	os.Exit(code)
}

var msg = dialog.WithBrand("OneDrive Helper - Settings")

func run(args []string) (int, string) {
	opt := strings.ToLower(strings.TrimLeft(args[0], "-/"))
	if opt == "h" || opt == "help" || opt == "?" {
		return 0, fmt.Sprintf(usage, version.Version)
	}
	// The settings window passes "--deadline <unix>" with its elevated
	// --import, so an approval that comes after the window gave up does nothing.
	var deadline time.Time
	if len(args) == 4 && opt == "import" && strings.EqualFold(strings.TrimLeft(args[2], "-/"), "deadline") {
		if n, err := strconv.ParseInt(args[3], 10, 64); err == nil {
			deadline = time.Unix(n, 0)
		}
		args = args[:2]
	}
	if len(args) != 2 {
		return 2, fmt.Sprintf(usage, version.Version)
	}
	path := args[1]
	switch opt {
	case "export":
		if err := config.Export(path); err != nil {
			return 1, "Export failed: " + err.Error()
		}
		return 0, "Settings exported to " + path
	case "validate":
		if _, _, err := config.ReadFile(path); err != nil {
			return 1, "Invalid settings file:\n" + err.Error()
		}
		return 0, "Settings file is valid."
	case "import":
		if !deadline.IsZero() && !time.Now().Before(deadline) {
			return 3, "The administrator approval came too late, so the settings were not saved."
		}
		if !winapi.IsElevated() {
			return 1, "--import must be run from an elevated (Run as administrator) prompt."
		}
		if err := config.Import(path); err != nil {
			return 1, "Import failed:\n" + err.Error()
		}
		return 0, "Settings imported from " + path
	}
	return 2, fmt.Sprintf(usage, version.Version)
}
