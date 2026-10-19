package version

// Version is overridden at build time via -ldflags "-X .../internal/version.Version=x.y.z".
var Version = "dev"

const (
	ProductName = "OneDrive Helper"
	ServiceName = "OneDriveHelper"
	EventSource = "OneDriveHelper"
	AgentExe    = "OneDriveHelperAgent.exe"
	ServiceExe  = "OneDriveHelper.exe"
	ConfigExe   = "OneDriveHelperConfig.exe"
)
