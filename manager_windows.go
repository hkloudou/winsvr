//go:build windows

package winsvr

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/eventlog"
	"golang.org/x/sys/windows/svc/mgr"
)

func startTypeToMgr(s StartType) uint32 {
	switch s {
	case StartManual:
		return mgr.StartManual
	case StartDisabled:
		return mgr.StartDisabled
	default:
		return mgr.StartAutomatic
	}
}

// Install registers the service with the Service Control Manager and installs
// an event-log source for it. It requires administrator rights.
func Install(c Config) error {
	if c.Name == "" {
		return errors.New("winsvr: Config.Name is required")
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("connect SCM: %w", err)
	}
	defer m.Disconnect()

	s, err := m.CreateService(c.Name, exe, mgr.Config{
		DisplayName:      c.displayName(),
		Description:      c.Description,
		StartType:        startTypeToMgr(c.StartType),
		DelayedAutoStart: c.DelayedAutoStart && c.StartType == StartAutomatic,
		Dependencies:     c.Dependencies,
		ServiceStartName: c.Account,
		Password:         c.Password,
	}, c.Arguments...)
	if err != nil {
		if errors.Is(err, windows.ERROR_SERVICE_EXISTS) {
			return fmt.Errorf("service %q is already installed", c.Name)
		}
		return fmt.Errorf("create service: %w", err)
	}
	defer s.Close()

	if c.RestartDelay > 0 {
		ra := mgr.RecoveryAction{Type: mgr.ServiceRestart, Delay: c.RestartDelay}
		if err := s.SetRecoveryActions([]mgr.RecoveryAction{ra, ra, ra}, 86400); err != nil {
			return fmt.Errorf("set recovery actions: %w", err)
		}
	}

	// InstallAsEventCreate registers the source against the generic
	// EventCreate.exe message file, so log entries render without a custom
	// .mc/.dll. Ignore "already exists".
	err = eventlog.InstallAsEventCreate(c.Name, eventlog.Error|eventlog.Warning|eventlog.Info)
	if err != nil && !errors.Is(err, os.ErrExist) {
		return fmt.Errorf("install event source: %w", err)
	}
	return nil
}

// Uninstall stops the service if needed, removes it from the SCM and removes
// its event-log source. It requires administrator rights.
func Uninstall(name string) error {
	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("connect SCM: %w", err)
	}
	defer m.Disconnect()

	s, err := m.OpenService(name)
	if err != nil {
		if errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST) {
			return fmt.Errorf("service %q is not installed", name)
		}
		return err
	}
	defer s.Close()

	// Best-effort stop; ignore "not running".
	_, _ = s.Control(svc.Stop)
	if err := s.Delete(); err != nil {
		return fmt.Errorf("delete service: %w", err)
	}
	if err := eventlog.Remove(name); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove event source: %w", err)
	}
	return nil
}

// Status reports the current state of the named service.
func Status(name string) (ServiceState, error) {
	m, err := mgr.Connect()
	if err != nil {
		return 0, err
	}
	defer m.Disconnect()
	s, err := m.OpenService(name)
	if err != nil {
		return 0, err
	}
	defer s.Close()
	st, err := s.Query()
	if err != nil {
		return 0, err
	}
	return ServiceState(st.State), nil
}

// Start starts an installed service via the SCM.
func Start(name string, args ...string) error {
	m, err := mgr.Connect()
	if err != nil {
		return err
	}
	defer m.Disconnect()
	s, err := m.OpenService(name)
	if err != nil {
		return err
	}
	defer s.Close()
	return s.Start(args...)
}

// Stop asks an installed service to stop.
func Stop(name string) error {
	m, err := mgr.Connect()
	if err != nil {
		return err
	}
	defer m.Disconnect()
	s, err := m.OpenService(name)
	if err != nil {
		return err
	}
	defer s.Close()
	_, err = s.Control(svc.Stop)
	return err
}
