//go:build !windows

package secrets

func newUserProtector() (Protector, error) { return nil, ErrProtectorUnavailable }
