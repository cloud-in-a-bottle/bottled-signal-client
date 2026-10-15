package backup

import (
	"archive/zip"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"path"
	"sort"
	"strings"

	"github.com/cloud-in-a-bottle/bottled-signal-client/internal/backup/backuppb"
)

// Archive is a local backup inside an uploaded zip: the SignalBackups folder, or a single snapshot folder with or
// without the shared files/ folder. The newest complete snapshot (a folder holding main and metadata) is used.
type Archive struct {
	zr    *zip.ReadCloser
	names map[string]*zip.File
	// Member-name prefixes ending in "/", or empty for the zip's root. filesPrefix is "" when there's no files/.
	snapshot    string
	filesPrefix string
	hasFiles    bool
}

func Open(zipPath string) (*Archive, error) {
	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		return nil, invalid("the upload isn't a valid zip file")
	}
	a := &Archive{zr: zr, names: make(map[string]*zip.File)}
	for _, f := range zr.File {
		if !f.FileInfo().IsDir() && !strings.HasPrefix(f.Name, "__MACOSX/") {
			a.names[f.Name] = f
		}
	}
	var snapshots []string
	for name := range a.names {
		if name != "metadata" && !strings.HasSuffix(name, "/metadata") {
			continue
		}
		prefix := strings.TrimSuffix(name, "metadata")
		if _, ok := a.names[prefix+"main"]; ok && !strings.HasSuffix(strings.TrimSuffix(prefix, "/"), "-tmp") {
			snapshots = append(snapshots, prefix)
		}
	}
	if len(snapshots) == 0 {
		_ = zr.Close()
		for name := range a.names {
			if strings.HasSuffix(name, ".backup") {
				return nil, invalid("this looks like a legacy Android .backup file, which isn't supported; export an " +
					"on-device backup (Settings › Chats › Backups) instead")
			}
		}
		return nil, invalid("couldn't find a backup (a folder containing 'main' and 'metadata') in the zip")
	}
	sort.SliceStable(snapshots, func(i, j int) bool { return folderName(snapshots[i]) < folderName(snapshots[j]) })
	a.snapshot = snapshots[len(snapshots)-1]
	parent := ""
	if trimmed := strings.TrimSuffix(a.snapshot, "/"); strings.Contains(trimmed, "/") {
		parent = path.Dir(trimmed) + "/"
	}
	for _, candidate := range []string{parent + "files/", a.snapshot + "files/", "files/"} {
		for name := range a.names {
			if strings.HasPrefix(name, candidate) {
				a.filesPrefix, a.hasFiles = candidate, true
				break
			}
		}
		if a.hasFiles {
			break
		}
	}
	return a, nil
}

// folderName is the last path element of a snapshot prefix, as Python's PurePosixPath(prefix).name.
func folderName(prefix string) string {
	trimmed := strings.TrimSuffix(prefix, "/")
	if trimmed == "" {
		return ""
	}
	return path.Base(trimmed)
}

func (a *Archive) Close() error {
	return a.zr.Close()
}

func (a *Archive) member(name string) *zip.File {
	return a.names[name]
}

func (a *Archive) attachment(mediaName string) *zip.File {
	if !a.hasFiles {
		return nil
	}
	for _, candidate := range []string{mediaName, strings.ToUpper(mediaName)} {
		if f := a.member(a.filesPrefix + candidate[:2] + "/" + candidate); f != nil {
			return f
		}
	}
	return nil
}

// WriteAttachment decrypts the local-backup file p refers to into w. found is false (with a nil error) if the
// pointer has no local locator or the file isn't in the zip. A file that fails its MAC or hash check is an error.
func (a *Archive) WriteAttachment(p *backuppb.FilePointer, w io.Writer) (bool, error) {
	loc := p.GetLocatorInfo()
	hashCheck, ok := loc.GetIntegrityCheck().(*backuppb.FilePointer_LocatorInfo_PlaintextHash)
	if loc == nil || loc.LocalKey == nil || !ok {
		return false, nil
	}
	f := a.attachment(LocalMediaName(hashCheck.PlaintextHash, loc.LocalKey))
	if f == nil {
		return false, nil
	}
	if len(loc.LocalKey) != 64 {
		return true, errors.New("attachment key has the wrong length")
	}
	aesKey, macKey := loc.LocalKey[:32], loc.LocalKey[32:]
	size := int64(f.UncompressedSize64)
	if err := verifyMAC(f.Open, size, macKey); err != nil {
		return true, fmt.Errorf("attachment %s is corrupt: %w", f.Name, err)
	}
	src, err := f.Open()
	if err != nil {
		return true, err
	}
	defer src.Close()
	plain, err := newCBCReader(src, size, aesKey)
	if err != nil {
		return true, err
	}
	digest := sha256.New()
	// The plaintext is padded with zeros beyond its size.
	n, err := io.Copy(io.MultiWriter(w, digest), io.LimitReader(plain, int64(loc.Size)))
	if err != nil {
		return true, err
	}
	if n != int64(loc.Size) || string(digest.Sum(nil)) != string(hashCheck.PlaintextHash) {
		return true, fmt.Errorf("attachment %s doesn't match its hash", f.Name)
	}
	return true, nil
}
