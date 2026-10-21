package agent

import (
	"time"

	"github.com/denismcapple/OneDriveHelper/internal/config"
	"github.com/denismcapple/OneDriveHelper/internal/onedrive"
	"github.com/denismcapple/OneDriveHelper/internal/popup"
)

// DemoConfig is a made-up organisation used for screenshots and previews.
func DemoConfig() config.Config {
	c := config.Defaults()
	c.CompanyName = "AcmeCorp"
	c.SupportName = "the AcmeCorp Service Desk"
	c.SupportEmail = "servicedesk@acmecorp.example"
	c.SupportPhone = "+1 555 0100"
	c.SupportURL = "https://help.acmecorp.example"
	return c
}

// DemoOrphans are made-up broken folders covering each group and re-sync case.
func DemoOrphans() []onedrive.Orphan {
	return []onedrive.Orphan{
		{Name: "Projects - Documents", Path: `C:\Users\jsmith\AcmeCorp\Projects - Documents`, TenantRoot: `C:\Users\jsmith\AcmeCorp`,
			Kind: onedrive.KindStopped, Library: &onedrive.Library{UserEmail: "jsmith@acmecorp.example", WebURL: "https://acmecorp.sharepoint.com/sites/projects"}},
		{Name: "Finance - Documents", Path: `C:\Users\jsmith\ACME CORPORATION LTD\Finance - Documents`, TenantRoot: `C:\Users\jsmith\ACME CORPORATION LTD`,
			Kind: onedrive.KindMoved, MovedTo: `C:\Users\jsmith\AcmeCorp\Finance - Documents`, WebURL: "https://acmecorp.sharepoint.com/sites/finance"},
		{Name: "Sales - Documents", Path: `C:\Users\jsmith\ACME CORPORATION LTD\Sales - Documents`, TenantRoot: `C:\Users\jsmith\ACME CORPORATION LTD`,
			Kind: onedrive.KindMoved, MovedTo: `C:\Users\jsmith\AcmeCorp\Sales - Documents`, WebURL: "https://acmecorp.sharepoint.com/sites/sales"},
		{Name: "Bids Team - Documents", Path: `C:\Users\jsmith\Fabrikam\Bids Team - Documents`, TenantRoot: `C:\Users\jsmith\Fabrikam`,
			Kind: onedrive.KindDisconnected, WebURL: "https://fabrikam.sharepoint.com/sites/bids"},
		{Name: "Contoso Ltd", Path: `C:\Users\jsmith\Contoso Ltd`, TenantRoot: `C:\Users\jsmith\Contoso Ltd`, EmptyOrg: true},
	}
}

// DocSpecs returns every card with made-up data, keyed by a file-friendly name.
func DocSpecs(c config.Config) map[string]popup.Spec {
	nop := Actions{Relaunch: func() {}, Snooze: func(string, string) {}, Support: func() {}, Email: func() {}, Website: func() {},
		Fix: func() {}, Explain: func(string) {}, CopyDetails: func() {}}
	orphans := DemoOrphans()
	broken := Status{At: time.Now(), Configured: true, Running: false, MissingSince: time.Now().Add(-7 * time.Minute), Orphans: orphans}
	healthy := Status{At: time.Now(), Configured: true, Running: true}
	return map[string]popup.Spec{
		"alert-onedrive-not-running": notRunningSpec(c, nop, true),
		"alert-folders-not-syncing":  orphanSpec(c, orphans, nop),
		"status-problems":            statusSpec(c, broken, nop),
		"status-all-good":            statusSpec(c, healthy, nop),
		"fix-in-progress":            fixingSpec(c),
	}
}
