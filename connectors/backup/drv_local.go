package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"filippo.io/age"
)

// fileTier is the shared shape of the two file drivers (local, rclone):
// each snapshot is one object, optionally age-encrypted (".age" suffix),
// with a sidecar ".<name>.meta" holding {sha256, size, enc} of the
// plaintext — itself age-encrypted when the tier is, so an encrypted tier
// leaks no content hash.
type fileTier struct {
	objects    objectStore
	recipients []age.Recipient
	identity   string
}

// objectStore is the minimal file backend: flat names under a set "dir".
type objectStore interface {
	write(ctx context.Context, set, name string, data []byte) error
	read(ctx context.Context, set, name string) ([]byte, error)
	names(ctx context.Context, set string) ([]string, error)
	del(ctx context.Context, set, name string) error
}

type meta struct {
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
	Enc    string `json:"enc,omitempty"`
}

func metaName(name string) string { return "." + name + ".meta" }

func (f *fileTier) encrypted() bool { return len(f.recipients) > 0 }

func (f *fileTier) seal(data []byte) ([]byte, error) {
	if !f.encrypted() {
		return data, nil
	}
	return ageEncrypt(f.recipients, data)
}

func (f *fileTier) put(ctx context.Context, set string, s snapshot, data []byte) error {
	obj := s.Name
	enc := ""
	if f.encrypted() {
		obj += ".age"
		enc = "age"
	}
	body, err := f.seal(data)
	if err != nil {
		return err
	}
	mb, _ := json.Marshal(meta{SHA256: s.SHA256, Size: s.Size, Enc: enc})
	if mb, err = f.seal(mb); err != nil {
		return err
	}
	// Meta first: a snapshot object is only ever listed once its meta exists
	// alongside it, so a half-finished put is never mistaken for a backup.
	if err := f.objects.write(ctx, set, metaName(s.Name), mb); err != nil {
		return err
	}
	return f.objects.write(ctx, set, obj, body)
}

func (f *fileTier) list(ctx context.Context, set string) ([]snapshot, error) {
	ns, err := f.objects.names(ctx, set)
	if err != nil {
		return nil, err
	}
	have := map[string]bool{}
	for _, n := range ns {
		have[n] = true
	}
	var out []snapshot
	for _, n := range ns {
		if strings.HasPrefix(n, ".") {
			continue
		}
		name, enc := n, ""
		if base, ok := strings.CutSuffix(n, ".age"); ok {
			name, enc = base, "age"
		}
		at, ok := parseName(set, name)
		if !ok || !have[metaName(name)] {
			continue
		}
		out = append(out, snapshot{Name: name, At: at, Size: -1, Enc: enc, ref: n})
	}
	sortNewest(out)
	return out, nil
}

// meta loads a snapshot's recorded sha256/size (decrypting when needed).
func (f *fileTier) meta(ctx context.Context, set string, s *snapshot) error {
	mb, err := f.objects.read(ctx, set, metaName(s.Name))
	if err != nil {
		return err
	}
	if s.Enc == "age" {
		if mb, err = ageDecrypt(f.identity, mb); err != nil {
			return err
		}
	}
	var m meta
	if err := json.Unmarshal(mb, &m); err != nil {
		return fmt.Errorf("meta for %s: %w", s.Name, err)
	}
	s.SHA256, s.Size = m.SHA256, m.Size
	return nil
}

func (f *fileTier) get(ctx context.Context, set string, s snapshot) ([]byte, error) {
	data, err := f.objects.read(ctx, set, s.ref)
	if err != nil {
		return nil, err
	}
	if s.Enc == "age" {
		return ageDecrypt(f.identity, data)
	}
	return data, nil
}

func (f *fileTier) remove(ctx context.Context, set string, ss []snapshot) error {
	for _, s := range ss {
		if err := f.objects.del(ctx, set, s.ref); err != nil {
			return err
		}
		if err := f.objects.del(ctx, set, metaName(s.Name)); err != nil {
			return err
		}
	}
	return nil
}

// check: every snapshot has a readable meta and an object whose size is
// consistent (plain: exact; encrypted: at least the plaintext size).
func (f *fileTier) check(ctx context.Context, set string) error {
	ss, err := f.list(ctx, set)
	if err != nil {
		return err
	}
	for _, s := range ss {
		if s.Enc == "age" && f.identity == "" {
			// Can't open the meta; existence was already established by list.
			continue
		}
		if err := f.meta(ctx, set, &s); err != nil {
			return err
		}
	}
	return nil
}

// --- local directory ---

type localObjects struct{ dir string }

func (l localObjects) setDir(set string) string { return filepath.Join(l.dir, set) }

func (l localObjects) write(_ context.Context, set, name string, data []byte) error {
	d := l.setDir(set)
	if err := os.MkdirAll(d, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(d, ".tmp-*")
	if err != nil {
		return err
	}
	_, werr := tmp.Write(data)
	serr := tmp.Sync()
	cerr := tmp.Close()
	if werr != nil || serr != nil || cerr != nil {
		_ = os.Remove(tmp.Name())
		return fmt.Errorf("write %s: %v %v %v", name, werr, serr, cerr)
	}
	if err := os.Rename(tmp.Name(), filepath.Join(d, name)); err != nil {
		_ = os.Remove(tmp.Name())
		return err
	}
	return nil
}

func (l localObjects) read(_ context.Context, set, name string) ([]byte, error) {
	return os.ReadFile(filepath.Join(l.setDir(set), name))
}

func (l localObjects) names(_ context.Context, set string) ([]string, error) {
	ents, err := os.ReadDir(l.setDir(set))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range ents {
		if e.Type().IsRegular() {
			out = append(out, e.Name())
		}
	}
	return out, nil
}

func (l localObjects) del(_ context.Context, set, name string) error {
	err := os.Remove(filepath.Join(l.setDir(set), name))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}
