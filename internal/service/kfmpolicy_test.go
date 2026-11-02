package service

import (
	"io"
	"log"
	"testing"

	"github.com/denismcapple/OneDriveHelper/internal/config"
	"github.com/denismcapple/OneDriveHelper/internal/kfm"
)

type fakeKFM struct {
	shared   []string
	tenants  []string
	applied  []string
	removed  int
	applyErr error
}

func (f *fakeKFM) env() kfmEnv {
	return kfmEnv{
		shared:  func() []string { return f.shared },
		tenants: func() []string { return f.tenants },
		apply:   func(t string) (bool, error) { f.applied = append(f.applied, t); return f.applyErr == nil, f.applyErr },
		remove:  func() (bool, error) { f.removed++; return true, nil },
	}
}

func TestManageKFM(t *testing.T) {
	h := &handler{log: log.New(io.Discard, "", 0)}
	cfg := config.Defaults()

	off := &fakeKFM{tenants: []string{"t1"}}
	h.manageKFM(cfg, off.env())
	if len(off.applied) != 0 || off.removed != 1 {
		t.Errorf("off: nothing set, and anything it set before is removed: %+v", off)
	}

	cfg.FolderRedirectionMigration = true
	rds := &fakeKFM{shared: []string{"this is a server, not a workstation"}, tenants: []string{"t1"}}
	h.manageKFM(cfg, rds.env())
	if len(rds.applied) != 0 || rds.removed != 1 {
		t.Errorf("never on a shared host: %+v", rds)
	}

	none := &fakeKFM{}
	h.manageKFM(cfg, none.env())
	if len(none.applied) != 0 {
		t.Errorf("no tenant yet: nothing set: %+v", none)
	}
	many := &fakeKFM{tenants: []string{"t1", "t2"}}
	h.manageKFM(cfg, many.env())
	if len(many.applied) != 0 {
		t.Errorf("several tenants: nothing set without KFMTenantID: %+v", many)
	}

	one := &fakeKFM{tenants: []string{"t1"}}
	h.manageKFM(cfg, one.env())
	if len(one.applied) != 1 || one.applied[0] != "t1" {
		t.Errorf("the signed-in user's tenant: %+v", one)
	}
	cfg.KFMTenantID = "11111111-2222-3333-4444-555555555555"
	set := &fakeKFM{tenants: []string{"t1"}}
	h.manageKFM(cfg, set.env())
	if len(set.applied) != 1 || set.applied[0] != cfg.KFMTenantID {
		t.Errorf("the configured tenant wins: %+v", set)
	}

	other := &fakeKFM{applyErr: kfm.ErrOtherPolicy}
	h.kfmLast = ""
	h.manageKFM(cfg, other.env())
	if h.kfmLast != "other" || other.removed != 0 {
		t.Errorf("another policy is left alone: last %q %+v", h.kfmLast, other)
	}
}
