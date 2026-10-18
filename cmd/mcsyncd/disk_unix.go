//go:build !windows

package main

import "syscall"

// freeSpace restituisce i byte liberi, per un utente normale, nel filesystem di path.
func freeSpace(path string) (uint64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, err
	}
	return uint64(st.Bavail) * uint64(st.Bsize), nil
}
