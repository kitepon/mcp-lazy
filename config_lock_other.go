//go:build !linux && !darwin

package main

import "fmt"

func lockConfig(path string) (func(), error) {
	return nil, fmt.Errorf("configuration changes are supported on Linux/macOS only")
}
