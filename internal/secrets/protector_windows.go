//go:build windows

package secrets

import (
	"runtime"
	"unsafe"

	"golang.org/x/sys/windows"
)

type dpapiProtector struct{}

func newUserProtector() (Protector, error) { return dpapiProtector{}, nil }

func (dpapiProtector) Name() string { return "Windows DPAPI (current user)" }

func (dpapiProtector) Protect(plaintext []byte) ([]byte, error) {
	return cryptData(plaintext, true)
}

func (dpapiProtector) Unprotect(ciphertext []byte) ([]byte, error) {
	return cryptData(ciphertext, false)
}

func cryptData(data []byte, protect bool) ([]byte, error) {
	in := windows.DataBlob{Size: uint32(len(data))}
	if len(data) > 0 {
		in.Data = &data[0]
	}
	var out windows.DataBlob
	var err error
	if protect {
		err = windows.CryptProtectData(&in, nil, nil, 0, nil, windows.CRYPTPROTECT_UI_FORBIDDEN, &out)
	} else {
		err = windows.CryptUnprotectData(&in, nil, nil, 0, nil, windows.CRYPTPROTECT_UI_FORBIDDEN, &out)
	}
	runtime.KeepAlive(data)
	if err != nil {
		return nil, err
	}
	if out.Data == nil {
		return []byte{}, nil
	}
	defer windows.LocalFree(windows.Handle(unsafe.Pointer(out.Data)))
	result := make([]byte, out.Size)
	copy(result, unsafe.Slice(out.Data, out.Size))
	return result, nil
}
