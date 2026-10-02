package selfupdate

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
)

var errMemberNotFound = errors.New("member not found")

// ExtractBinary writes the one archive entry whose base name is member
// ("tenant", or "tenant.exe" on Windows; the release archives are flat) to
// destPath through a temporary file beside it, mode 0755. Entries are
// checked as they are met: an absolute name or one that climbs with ".." is
// refused before anything is written, and so is its archive, since a release
// of ours never carries one. The entry streams, so a large binary costs no
// memory.
func ExtractBinary(archivePath, destPath, member string) error {
	var err error
	switch {
	case strings.HasSuffix(archivePath, ".zip"):
		err = extractZip(archivePath, destPath, member)
	case strings.HasSuffix(archivePath, ".tar.gz"), strings.HasSuffix(archivePath, ".tgz"):
		err = extractTarGz(archivePath, destPath, member)
	default:
		return fmt.Errorf("%s: not a .tar.gz or .zip archive", archivePath)
	}
	if errors.Is(err, errMemberNotFound) {
		return fmt.Errorf("%s: no entry named %s", filepath.Base(archivePath), member)
	}
	return err
}

// wanted decides one entry: refused (err), the member (true) or skipped.
// Zip and tar names use "/", but a hostile zip may carry "\"; both are
// treated as separators so "..\x" cannot slip past as one odd element.
func wanted(name, member string) (bool, error) {
	clean := strings.ReplaceAll(name, `\`, "/")
	if strings.HasPrefix(clean, "/") || filepath.VolumeName(name) != "" || slices.Contains(strings.Split(clean, "/"), "..") {
		return false, fmt.Errorf("refusing archive entry %q: absolute or escaping name", name)
	}
	return path.Base(clean) == member, nil
}

func extractTarGz(archivePath, destPath, member string) error {
	f, err := os.Open(archivePath)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("%s: %w", archivePath, err)
	}
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		switch {
		case errors.Is(err, io.EOF):
			return errMemberNotFound
		case errors.Is(err, tar.ErrInsecurePath): // GODEBUG tarinsecurepath=0 reports what wanted refuses anyway
			return fmt.Errorf("refusing archive entry %q: %w", hdr.Name, err)
		case err != nil:
			return err
		}
		hit, err := wanted(hdr.Name, member)
		if err != nil {
			return err
		}
		if hit && hdr.Typeflag == tar.TypeReg {
			return writeAtomic(destPath, 0o755, tr)
		}
	}
}

func extractZip(archivePath, destPath, member string) error {
	zr, err := zip.OpenReader(archivePath)
	if errors.Is(err, zip.ErrInsecurePath) { // GODEBUG zipinsecurepath=0 reports what wanted refuses anyway
		if zr != nil {
			_ = zr.Close()
		}
		return fmt.Errorf("refusing %s: %w", filepath.Base(archivePath), err)
	}
	if err != nil {
		return err
	}
	defer zr.Close()
	for _, zf := range zr.File {
		hit, err := wanted(zf.Name, member)
		if err != nil {
			return err
		}
		if !hit || zf.FileInfo().IsDir() {
			continue
		}
		rc, err := zf.Open()
		if err != nil {
			return err
		}
		err = writeAtomic(destPath, 0o755, rc)
		_ = rc.Close()
		return err
	}
	return errMemberNotFound
}
