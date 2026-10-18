//go:build windows

package main

import "errors"

// Il servizio gira su Linux: su Windows il controllo dello spazio non c'è.
func freeSpace(string) (uint64, error) {
	return 0, errors.New("controllo dello spazio non disponibile su Windows")
}
