//go:build !linux

package agent

func hideProcess() error { return nil }
