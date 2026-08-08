//go:build windows

package sandbox

import (
	"fmt"

	"golang.org/x/sys/windows"
)

// mergeAccessEntry applies entry to path's access list, merged with the list already
// there so nothing that had access loses it. Every grant and revoke this package
// performs is the same read-modify-write against a named object, and the entry is the
// only part that varies: the mask, the inheritance and whether it sets or revokes.
//
// Merging rather than replacing is what makes a grant idempotent. windows.ACLFromEntries
// resolves a second entry for a trustee already present by replacing that trustee's
// entries, so re-granting on every launch does not stack duplicates, and a REVOKE_ACCESS
// entry drops the trustee's entries and leaves every other one intact.
func mergeAccessEntry(path string, entry windows.EXPLICIT_ACCESS) error {
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return fmt.Errorf("read access list: %w", err)
	}
	existing, _, err := sd.DACL()
	if err != nil {
		return fmt.Errorf("access list: %w", err)
	}
	merged, err := windows.ACLFromEntries([]windows.EXPLICIT_ACCESS{entry}, existing)
	if err != nil {
		return fmt.Errorf("merge access list: %w", err)
	}
	if err := windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION, nil, nil, merged, nil); err != nil {
		return fmt.Errorf("apply access list: %w", err)
	}
	return nil
}

// sidTrustee names sid as the subject of an access entry. kind is the trustee type the
// entry claims sid is: the per-workspace container identities are groups, and the
// well-known all-application-packages sid must be declared as a well-known group.
func sidTrustee(sid *windows.SID, kind windows.TRUSTEE_TYPE) windows.TRUSTEE {
	return windows.TRUSTEE{
		TrusteeForm:  windows.TRUSTEE_IS_SID,
		TrusteeType:  kind,
		TrusteeValue: windows.TrusteeValueFromSID(sid),
	}
}
