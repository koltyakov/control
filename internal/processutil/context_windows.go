package processutil

import "golang.org/x/sys/windows"

// RunAs reports "system" for the built-in service accounts, such as the
// LocalService SCM service, and "user" for an interactive account, including
// an elevated one. It returns "" when the token cannot be read.
func RunAs() string {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return ""
	}
	for _, sid := range []windows.WELL_KNOWN_SID_TYPE{windows.WinLocalSystemSid, windows.WinLocalServiceSid, windows.WinNetworkServiceSid} {
		if user.User.Sid.IsWellKnown(sid) {
			return "system"
		}
	}
	return "user"
}
