package agent

import (
	"strings"

	"github.com/denismcapple/OneDriveHelper/internal/config"
	"github.com/denismcapple/OneDriveHelper/internal/onedrive"
)

// Help topics shown by the "What does this mean?" link on each alert.
const (
	topicNotRunning = "not-running"
	topicOrphans    = "orphans"
	topicStatus     = "status"
)

const explainTitle = "What does this mean?"

// Explain returns the plain-English explanation for a topic, written for
// people who don't know (or care) how OneDrive works.
func Explain(c config.Config, topic string, orphans []onedrive.Orphan) string {
	switch topic {
	case topicNotRunning:
		return "WHAT HAS HAPPENED?\n" +
			"OneDrive is the program that copies your work files to the cloud, so they are kept safe and your colleagues can see them. " +
			"It has stopped running on this computer. This often happens after it updates itself and doesn't start again.\n\n" +
			"WHY ARE WE TELLING YOU?\n" +
			"While OneDrive is stopped, nothing you save is copied to the cloud, and there is nothing else on screen to warn you.\n\n" +
			"WHY DOES IT NEED FIXING?\n" +
			"•  Your latest work is only on this computer.\n" +
			"•  Colleagues won't see your changes, and you won't see theirs.\n" +
			"•  If this computer breaks, is lost or is stolen, your recent work could be gone for good.\n\n" +
			"WHAT SHOULD I DO?\n" +
			"Click 'Restart OneDrive'. It takes a few seconds, it is completely safe, and you won't lose anything. " +
			"If this message keeps coming back: " + c.SupportContact()
	case topicOrphans:
		var list strings.Builder
		for _, o := range orphans {
			list.WriteString("•  " + o.DisplayName() + "\n")
		}
		if list.Len() > 0 {
			list.WriteString("\n")
		}
		return "WHAT HAS HAPPENED?\n" +
			"Some folders on this computer used to be linked to shared company folders (in SharePoint or Teams). " +
			"That link has been broken, but the folders are still here, so they look perfectly normal:\n\n" +
			list.String() +
			"WHY ARE WE TELLING YOU?\n" +
			"Anything you save or change in these folders now stays ONLY on this computer. It is not backed up and your colleagues can't see it. " +
			"Windows and OneDrive don't warn you about this, so we do.\n\n" +
			"WHY DOES IT NEED FIXING?\n" +
			"•  Work saved in these folders is not backed up.\n" +
			"•  You may be working on old copies while colleagues work on newer ones.\n" +
			"•  If this computer breaks, is lost, stolen or replaced, anything saved there is lost.\n\n" +
			"WHAT SHOULD I DO?\n" +
			"For now, avoid saving new work in these folders. " + c.SupportContact() + " " +
			"If your support team asks you to, click 'Fix (admin)': it saves a copy of all your files first, so nothing is lost."
	default:
		return "This window shows whether OneDrive is working properly on this computer.\n\n" +
			"OneDrive copies your work files to the cloud, so they are backed up and shared with your colleagues. " +
			"If anything stops that from happening, it is listed here, together with what can be done about it.\n\n" +
			"If your support team asks for details, click 'Copy details' and paste them into your e-mail or ticket.\n\n" +
			c.SupportContact()
	}
}
