package ipc

import (
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestTwoHoldersKeepTheLockVisible(t *testing.T) {
	name := fmt.Sprintf(`Global\OneDriveHelperFixingTest-%d`, time.Now().UnixNano())
	if exists(name) {
		t.Fatal("should not exist yet")
	}
	r1, err := holdNamed(name) // e.g. the agent in session 1
	if err != nil {
		t.Fatal(err)
	}
	r2, err := holdNamed(name) // and in session 2
	if err != nil {
		t.Fatal(err)
	}
	if !exists(name) {
		t.Fatal("lock should be visible")
	}
	r1()
	if !exists(name) {
		t.Fatal("lock must stay visible while the second holder has it")
	}
	r2()
	if exists(name) {
		t.Fatal("lock should be gone once both released")
	}
}

func TestNames(t *testing.T) {
	if AgentMutexForSession(3) != `Session\3\OneDriveHelperAgent` || AgentMutexLocal() != `Local\OneDriveHelperAgent` {
		t.Error("agent mutex names changed; the service and agent must agree")
	}
	if !strings.HasPrefix(StopEventName(1, 2), `Global\OneDriveHelperStop-`) {
		t.Error("stop event name")
	}
}

// The LocalSystem service must not link the agent's UI/remediation code.
func TestServiceDoesNotImportAgent(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", "github.com/denismcapple/OneDriveHelper/internal/service").Output()
	if err != nil {
		t.Skipf("go list: %v", err)
	}
	for _, pkg := range []string{"internal/agent", "internal/popup", "internal/remediate"} {
		if strings.Contains(string(out), "OneDriveHelper/"+pkg+"\n") {
			t.Errorf("internal/service depends on %s", pkg)
		}
	}
}
