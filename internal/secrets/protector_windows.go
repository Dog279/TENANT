//go:build windows

package secrets

import (
	"runtime"
	"unsafe"

	"golang.org/x/sys/windows"
)

// dpapiProtector seals to the current user, or to the machine on a machine
// install (permissions_windows.go): there the service and the operator are
// two accounts that must open each other's blobs, and the file's DACL, which
// names both, is what keeps everyone else out.
type dpapiProtector struct{ machine bool }

func newUserProtector() (Protector, error) {
	id, err := currentIdentity()
	if err != nil {
		return nil, err
	}
	return dpapiProtector{machine: id.machine()}, nil
}

func (p dpapiProtector) Name() string {
	if p.machine {
		return "Windows DPAPI (local machine)"
	}
	return "Windows DPAPI (current user)"
}

func (p dpapiProtector) Protect(plaintext []byte) ([]byte, error) {
	flags := uint32(windows.CRYPTPROTECT_UI_FORBIDDEN)
	if p.machine {
		flags |= windows.CRYPTPROTECT_LOCAL_MACHINE
	}
	return cryptData(plaintext, true, flags)
}

// Unprotect opens a blob of either scope: the blob records its own.
func (dpapiProtector) Unprotect(ciphertext []byte) ([]byte, error) {
	return cryptData(ciphertext, false, windows.CRYPTPROTECT_UI_FORBIDDEN)
}

func cryptData(data []byte, protect bool, flags uint32) ([]byte, error) {
	in := windows.DataBlob{Size: uint32(len(data))}
	if len(data) > 0 {
		in.Data = &data[0]
	}
	var out windows.DataBlob
	var err error
	if protect {
		err = windows.CryptProtectData(&in, nil, nil, 0, nil, flags, &out)
	} else {
		err = windows.CryptUnprotectData(&in, nil, nil, 0, nil, flags, &out)
	}
	runtime.KeepAlive(data)
	if err != nil {
		return nil, err
	}
	if out.Data == nil {
		return []byte{}, nil
	}
	defer windows.LocalFree(windows.Handle(unsafe.Pointer(out.Data)))
	theirs := unsafe.Slice(out.Data, out.Size)
	result := make([]byte, out.Size)
	copy(result, theirs)
	// When unprotecting, theirs is the secret: zero it, or it stays readable
	// in freed memory after the caller has wiped result.
	clear(theirs)
	return result, nil
}
