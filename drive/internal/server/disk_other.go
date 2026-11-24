//go:build !linux

package server

func diskStats(string) (total, free uint64, ok bool) { return 0, 0, false }
