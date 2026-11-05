// native-backup-proof verifies one retained native snapshot using an existing
// host's sealed storage profile. It never opens an agent manager/listener,
// changes settings or exports the profile. Run with a protected temporary dir
// and a supervisor deadline; stdin must be the complete already-restored dump.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/agim/lidza/packs/storage"
	"github.com/agim/lidza/pkg/credentials"
)

const archiveLimit = 1 << 30

type expectation struct {
	Endpoint, Bucket, Region, Prefix, Key, SHA256 string
	Size                                          int64
}

type receipt struct {
	VerifiedUTC          time.Time             `json:"verified_utc"`
	ObjectKey            string                `json:"object_key"`
	Size                 int64                 `json:"bytes"`
	SHA256               string                `json:"sha256"`
	UploadSHA256         string                `json:"upload_sha256"`
	ReadbackSHA256       string                `json:"readback_sha256"`
	Privacy              storage.PrivacyReport `json:"privacy"`
	CompleteReadback     bool                  `json:"complete_readback"`
	ConfigurationChanged bool                  `json:"configuration_changed"`
	CredentialsExported  bool                  `json:"credentials_exported"`
}

func main() {
	var x expectation
	dir := flag.String("state-dir", "", "existing encrypted agent state directory (read-only)")
	flag.StringVar(&x.Endpoint, "endpoint", "", "expected existing HTTPS storage endpoint")
	flag.StringVar(&x.Bucket, "bucket", "", "expected existing bucket")
	flag.StringVar(&x.Region, "region", "", "expected existing region")
	flag.StringVar(&x.Prefix, "prefix", "", "expected existing prefix")
	flag.StringVar(&x.Key, "key", "", "fresh native-proof key with a random 32-hex identity")
	flag.StringVar(&x.SHA256, "sha256", "", "checksum of the already-restored archive")
	flag.Int64Var(&x.Size, "size", 0, "exact archive size")
	flag.Parse()
	if err := validateExpectation(x); err != nil {
		fail(err)
	}
	cfg, err := readProfile(*dir, x)
	if err != nil {
		fail(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
	defer cancel()
	r, err := prove(ctx, cfg, x, os.Stdin, os.TempDir())
	if err != nil {
		fail(err)
	}
	if json.NewEncoder(os.Stdout).Encode(r) != nil {
		fail(errors.New("could not write proof receipt"))
	}
}

func fail(err error) { fmt.Fprintln(os.Stderr, err); os.Exit(1) }

func validateExpectation(x expectation) error {
	if !strings.HasPrefix(x.Endpoint, "https://") || x.Bucket == "" || x.Region == "" || x.Prefix == "" || !regexp.MustCompile(`^native-proof/[a-z0-9][a-z0-9-]{0,63}/[a-f0-9]{32}\.dump$`).MatchString(x.Key) || !regexp.MustCompile(`^[a-f0-9]{64}$`).MatchString(x.SHA256) || x.Size <= 0 || x.Size > archiveLimit {
		return errors.New("exact bounded archive/storage expectations are required")
	}
	return nil
}

func boundedRegular(path string, limit int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, errors.New("retained storage state is unavailable")
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > limit || info.Mode().Perm()&0077 != 0 {
		return nil, errors.New("retained storage state must be a bounded private regular file")
	}
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil || int64(len(data)) > limit {
		return nil, errors.New("retained storage state cannot be read")
	}
	return data, nil
}

func readProfile(dir string, x expectation) (storage.Config, error) {
	var empty storage.Config
	if !filepath.IsAbs(dir) {
		return empty, errors.New("an absolute retained state directory is required")
	}
	// Read the explicit retained key, never an environment override or generator.
	rawKey, err := boundedRegular(filepath.Join(dir, "config/master.key"), 128)
	if err != nil {
		return empty, err
	}
	key, err := hex.DecodeString(strings.TrimSpace(string(rawKey)))
	if err != nil || len(key) != 32 {
		return empty, errors.New("retained key is invalid")
	}
	raw, err := boundedRegular(filepath.Join(dir, "state.json"), 128<<20)
	if err != nil {
		return empty, err
	}
	var sealed string
	if json.Unmarshal(raw, &sealed) != nil {
		return empty, errors.New("retained state must be authenticated ciphertext")
	}
	plain, err := credentials.Decrypt(key, sealed)
	if err != nil {
		return empty, errors.New("retained state authentication failed")
	}
	var snapshot struct {
		BackupStorage *storage.Config `json:"backup_storage"`
	}
	if json.Unmarshal(plain, &snapshot) != nil || snapshot.BackupStorage == nil {
		return empty, errors.New("retained storage profile is unavailable")
	}
	cfg := *snapshot.BackupStorage
	if cfg.Provider != "s3" || cfg.Endpoint != x.Endpoint || cfg.Bucket != x.Bucket || cfg.Region != x.Region || cfg.Prefix != x.Prefix || cfg.AccessKey == "" || cfg.SecretKey == "" {
		return empty, errors.New("retained storage profile does not match the reviewed destination")
	}
	cfg.Timeout = 30 * time.Second
	return cfg, nil
}

func prove(ctx context.Context, cfg storage.Config, x expectation, input io.Reader, tempRoot string) (receipt, error) {
	var r receipt
	// Verify all incoming bytes before giving the storage provider any DB data.
	// This temporary copy stays on the credential-holding host, never the client.
	dir, err := os.MkdirTemp(tempRoot, "lidza-native-proof-")
	if err != nil {
		return r, errors.New("cannot create private snapshot spool")
	}
	defer os.RemoveAll(dir)
	f, err := os.OpenFile(filepath.Join(dir, "snapshot.dump"), os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err != nil {
		return r, errors.New("cannot create private snapshot spool")
	}
	defer f.Close()
	hash := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, hash), io.LimitReader(input, x.Size+1))
	if err != nil || n != x.Size || hex.EncodeToString(hash.Sum(nil)) != x.SHA256 {
		return r, errors.New("incoming archive size or checksum differs; nothing uploaded")
	}
	if err = f.Sync(); err != nil {
		return r, errors.New("snapshot spool sync failed")
	}
	store, err := storage.New(cfg)
	if err != nil {
		return r, errors.New("cannot open retained storage profile")
	}
	if _, err = store.Stat(ctx, x.Key); !errors.Is(err, storage.ErrNotFound) {
		return r, errors.New("proof key exists or absence cannot be verified; nothing overwritten")
	}
	// The released framework probes the exact future key using harmless bytes,
	// checks anonymous object/listing/CDN access and removes only that probe.
	privacy, err := store.PrivacyProbe(ctx, x.Key)
	if err != nil || !privacy.Private || privacy.Public {
		return r, errors.New("exact-key privacy probe did not verify denial; snapshot not uploaded")
	}
	if _, err = store.Stat(ctx, x.Key); !errors.Is(err, storage.ErrNotFound) {
		return r, errors.New("temporary probe removal was not verified; snapshot not uploaded")
	}
	signed, err := store.PresignPut(ctx, x.Key, 15*time.Minute, "application/octet-stream")
	if err != nil {
		return r, errors.New("snapshot upload authorization failed")
	}
	if _, err = f.Seek(0, io.SeekStart); err != nil {
		return r, errors.New("snapshot spool rewind failed")
	}
	transmitted := sha256.New()
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, signed, io.TeeReader(f, transmitted))
	if err != nil {
		return r, errors.New("snapshot upload destination is invalid")
	}
	req.ContentLength = x.Size
	req.Header.Set("Content-Type", "application/octet-stream")
	// Never overwrite a key created since the probe. No retry/redirect fallback.
	req.Header.Set("If-None-Match", "*")
	client := &http.Client{Timeout: 10 * time.Minute, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	res, err := client.Do(req)
	if err != nil {
		return r, errors.New("snapshot upload failed; local archive retained at its source")
	}
	res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 || hex.EncodeToString(transmitted.Sum(nil)) != x.SHA256 {
		return r, errors.New("snapshot upload was refused or checksum differs; no proof recorded")
	}
	privacy, err = store.Privacy(ctx, x.Key)
	if err != nil || !privacy.Private || privacy.Public {
		if privacy.Public {
			_ = store.Delete(context.WithoutCancel(ctx), x.Key)
		}
		return r, errors.New("uploaded snapshot privacy was not verified; no proof recorded")
	}
	body, object, err := store.Get(ctx, x.Key)
	if err != nil {
		return r, errors.New("off-site snapshot readback failed")
	}
	defer body.Close()
	readback := sha256.New()
	n, err = io.Copy(readback, io.LimitReader(body, x.Size+1))
	if err != nil || n != x.Size || object.Size != x.Size || hex.EncodeToString(readback.Sum(nil)) != x.SHA256 {
		return r, errors.New("complete off-site snapshot checksum or size differs")
	}
	return receipt{VerifiedUTC: time.Now().UTC(), ObjectKey: x.Key, Size: n, SHA256: x.SHA256, UploadSHA256: hex.EncodeToString(transmitted.Sum(nil)), ReadbackSHA256: hex.EncodeToString(readback.Sum(nil)), Privacy: privacy, CompleteReadback: true}, nil
}
