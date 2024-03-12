//go:build !linux
// +build !linux

package routemanager

import (
	"fmt"
	"net/netip"
	"os/exec"
	"runtime"

	log "github.com/sirupsen/logrus"
)

func setupRouting() error {
	return nil
}

func cleanupRouting() error {
	return nil
}

func addToRouteTable(prefix netip.Prefix, addr string) error {
	cmd := exec.Command("route", "add", prefix.String(), addr)
	out, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("add route: %w", err)
	}
	log.Debugf(string(out))
	return nil
}

func removeFromRouteTable(prefix netip.Prefix, addr string) error {
	args := []string{"delete", prefix.String()}
	if runtime.GOOS == "darwin" {
		args = append(args, addr)
	}
	cmd := exec.Command("route", args...)
	out, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("remove route: %w", err)
	}
	log.Debugf(string(out))
	return nil
}

func enableIPForwarding() error {
	log.Infof("Enable IP forwarding is not implemented on %s", runtime.GOOS)
	return nil
}
