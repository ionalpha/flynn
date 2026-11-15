//go:build windows

package sandbox

import (
	"fmt"

	"golang.org/x/sys/windows"
)

// mergeAccessEntry merges entry into path's list so nothing loses access. The
// merge makes a grant idempotent: windows.ACLFromEntries replaces a trustee's
// existing entries, so re-granting does not stack, and REVOKE_ACCESS drops only
// that trustee's.
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

// sidTrustee takes kind because container identities are groups and ALL
// APPLICATION PACKAGES must be declared a well-known group.
func sidTrustee(sid *windows.SID, kind windows.TRUSTEE_TYPE) windows.TRUSTEE {
	return windows.TRUSTEE{
		TrusteeForm:  windows.TRUSTEE_IS_SID,
		TrusteeType:  kind,
		TrusteeValue: windows.TrusteeValueFromSID(sid),
	}
}
