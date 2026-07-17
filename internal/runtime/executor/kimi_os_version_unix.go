//go:build !windows

package executor

import "golang.org/x/sys/unix"

func getKimiOSVersion() string {
	var utsname unix.Utsname
	if err := unix.Uname(&utsname); err != nil {
		return "unknown"
	}
	return unix.ByteSliceToString(utsname.Release[:])
}
