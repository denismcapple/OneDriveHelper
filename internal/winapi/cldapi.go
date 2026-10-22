package winapi

import (
	"encoding/binary"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	cldapi                      = windows.NewLazySystemDLL("cldapi.dll")
	procCfGetSyncRootInfoByPath = cldapi.NewProc("CfGetSyncRootInfoByPath")
	procCfUnregisterSyncRoot    = cldapi.NewProc("CfUnregisterSyncRoot")
)

// SyncProviderDisconnected is CF_SYNC_PROVIDER_STATUS_DISCONNECTED: no sync
// program (e.g. OneDrive) is connected to the sync root.
const SyncProviderDisconnected = 0

// SyncRoot is what Windows' Cloud Files driver knows about the sync root a
// path is in.
type SyncRoot struct {
	FileID   int64  // file ID of the sync root folder
	Status   uint32 // CF_SYNC_PROVIDER_STATUS
	Provider string // e.g. "OneDrive - Business"
}

// SyncRootInfo asks the Cloud Files driver which sync root path is in. The
// driver keeps a sync root's registration on the folder itself, so this also
// finds sync roots with no entry left in the registry.
func SyncRootInfo(path string) (SyncRoot, error) {
	if err := procCfGetSyncRootInfoByPath.Find(); err != nil {
		return SyncRoot{}, err
	}
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return SyncRoot{}, err
	}
	// CF_SYNC_ROOT_STANDARD_INFO: SyncRootFileId(8) HydrationPolicy(4)
	// PopulationPolicy(4) InSyncPolicy(4) HardLinkPolicy(4)
	// ProviderStatus(4) ProviderName[256]WCHAR ProviderVersion[64]WCHAR
	// SyncRootIdentityLength(4) SyncRootIdentity[...]
	buf := make([]byte, 64<<10)
	var n uint32
	r, _, _ := procCfGetSyncRootInfoByPath.Call(uintptr(unsafe.Pointer(p)), 1, // CF_SYNC_ROOT_INFO_STANDARD
		uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)), uintptr(unsafe.Pointer(&n)))
	if r != 0 {
		return SyncRoot{}, windows.Errno(r & 0xFFFF)
	}
	name := make([]uint16, 256)
	for i := range name {
		name[i] = binary.LittleEndian.Uint16(buf[28+2*i:])
	}
	return SyncRoot{
		FileID:   int64(binary.LittleEndian.Uint64(buf[0:])),
		Status:   binary.LittleEndian.Uint32(buf[24:]),
		Provider: windows.UTF16ToString(name),
	}, nil
}

// UnregisterSyncRoot removes the Cloud Files driver's registration of the
// sync root folder at path. Files are not touched.
func UnregisterSyncRoot(path string) error {
	if err := procCfUnregisterSyncRoot.Find(); err != nil {
		return err
	}
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	if r, _, _ := procCfUnregisterSyncRoot.Call(uintptr(unsafe.Pointer(p))); r != 0 {
		return windows.Errno(r & 0xFFFF)
	}
	return nil
}
