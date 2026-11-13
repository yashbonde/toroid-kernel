//go:build android

package toroid

import (
	"context"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Android fixups for a pure-Go (CGO_ENABLED=0) build, applied when the package
// loads. Each one only fills a gap and leaves explicit configuration alone:
//
//   - DNS: Android has no /etc/resolv.conf, so Go's resolver would query
//     [::1]:53 and every lookup fails. Queries go to TOROID_DNS (comma-separated
//     host:port list) or, by default, 1.1.1.1 then 8.8.8.8.
//   - Time zone: Go's runtime always starts in UTC on Android and ignores TZ.
//     time.Local is set from TZ when given, else from the device's
//     persist.sys.timezone property.
//   - Data directory: under adb shell HOME is "/", which is not writable. When
//     TOROID_HOME is unset and ~/.toroid cannot be created, TOROID_HOME is set to
//     $TMPDIR/.toroid (TMPDIR defaults to /data/local/tmp).
func init() {
	androidDNS()
	androidTimeZone()
	androidHome()
}

func androidDNS() {
	if _, err := os.Stat("/etc/resolv.conf"); err == nil {
		return // a resolv.conf exists (e.g. Termux, a chroot): the stock resolver works
	}
	servers := []string{"1.1.1.1:53", "8.8.8.8:53"}
	if v := strings.TrimSpace(os.Getenv("TOROID_DNS")); v != "" {
		servers = strings.Split(v, ",")
	}
	net.DefaultResolver = &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
			d := net.Dialer{Timeout: 3 * time.Second}
			var err error
			for _, s := range servers {
				var c net.Conn
				if c, err = d.DialContext(ctx, network, strings.TrimSpace(s)); err == nil {
					return c, nil
				}
			}
			return nil, err
		},
	}
}

func androidTimeZone() {
	name := strings.TrimSpace(os.Getenv("TZ"))
	if name == "" {
		out, err := exec.Command("getprop", "persist.sys.timezone").Output()
		if err != nil {
			return
		}
		name = strings.TrimSpace(string(out))
	}
	if loc, err := time.LoadLocation(name); err == nil {
		time.Local = loc
	}
}

func androidHome() {
	if os.Getenv("TOROID_HOME") != "" {
		return
	}
	if home, err := os.UserHomeDir(); err == nil && os.MkdirAll(filepath.Join(home, ".toroid"), 0755) == nil {
		return
	}
	_ = os.Setenv("TOROID_HOME", filepath.Join(os.TempDir(), ".toroid"))
}
