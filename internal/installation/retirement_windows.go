package installation

import (
	"context"
	"errors"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc/mgr"
)

func launchRetiredUninstall(_ context.Context, helper string, args []string, _ installedNode) error {
	return launchDetachedUninstall(helper, args)
}

func removeRetiredStartup(ctx context.Context, p installedNode) error {
	if p.Mode == "process" {
		return nil
	}
	if p.Mode == "user" {
		return removeUserTask(ctx, p.Config)
	}
	// Use only the rights delegated to this service's SID during installation.
	// mgr.Connect/OpenService request administrative access that LocalService lacks.
	handle, err := windows.OpenSCManager(nil, nil, windows.SC_MANAGER_CONNECT)
	if err != nil {
		return err
	}
	defer func() { _ = windows.CloseServiceHandle(handle) }()
	name, err := windows.UTF16PtrFromString(WindowsServiceName(p.Config))
	if err != nil {
		return err
	}
	h, err := windows.OpenService(handle, name, windows.DELETE|windows.SERVICE_QUERY_CONFIG)
	if errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST) || errors.Is(err, windows.ERROR_SERVICE_MARKED_FOR_DELETE) {
		return nil
	}
	if err != nil {
		return err
	}
	s := &mgr.Service{Name: WindowsServiceName(p.Config), Handle: h}
	defer func() { _ = s.Close() }()
	settings, err := s.Config()
	if err != nil {
		return err
	}
	args, err := windows.DecomposeCommandLine(settings.BinaryPathName)
	if err != nil {
		return err
	}
	if len(args) != 3 || !strings.EqualFold(filepath.Clean(args[0]), p.Binary) || args[1] != "__service" || !strings.EqualFold(filepath.Clean(args[2]), p.Config) {
		return errors.New("system service belongs to another installation")
	}
	return s.Delete()
}

func finishRetiredUninstall(_ context.Context, _ installedNode) error { return nil }

func finishRetiredHelper(_ context.Context, _, _ string) {}

func grantServiceSelfRemoval(ctx context.Context, s *mgr.Service, binary, config string) error {
	sid, _, _, err := windows.LookupSID("", `NT SERVICE\`+s.Name)
	if err != nil {
		return err
	}
	sd, err := windows.GetSecurityInfo(s.Handle, windows.SE_SERVICE, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return err
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return err
	}
	acl, err := windows.ACLFromEntries([]windows.EXPLICIT_ACCESS{{
		AccessPermissions: windows.DELETE | windows.SERVICE_QUERY_CONFIG,
		AccessMode:        windows.GRANT_ACCESS,
		Trustee: windows.TRUSTEE{TrusteeForm: windows.TRUSTEE_IS_SID, TrusteeType: windows.TRUSTEE_IS_USER,
			TrusteeValue: windows.TrusteeValueFromSID(sid)},
	}}, dacl)
	if err != nil {
		return err
	}
	if err = windows.SetSecurityInfo(s.Handle, windows.SE_SERVICE, windows.DACL_SECURITY_INFORMATION, nil, nil, acl, nil); err != nil {
		return err
	}
	p := installedNode{Binary: binary, Config: config}
	for _, path := range []string{binary, binaryRegistration(p)} {
		if err = run(ctx, "icacls.exe", path, "/grant", "*"+sid.String()+":(RX,D)", "/Q"); err != nil {
			return err
		}
	}
	if err = run(ctx, "icacls.exe", filepath.Join(binary+".profiles", "registry.lock"), "/grant", "*"+sid.String()+":(M)", "/Q"); err != nil {
		return err
	}
	return run(ctx, "icacls.exe", binary+".profiles", "/grant", "*"+sid.String()+":(RX)", "/Q")
}
