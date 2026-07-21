package config

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// committedConfig returns a Config backed by a temp file with persistence on,
// which is the state the daemon runs in after setup.
func committedConfig(t *testing.T) (*Config, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "relume-tv.json")
	c, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := c.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	return c, path
}

func TestPSKForUser_decodesTheHexClientKey(t *testing.T) {
	// Given: a paired TV whose clientkey is the 16-byte DTLS PSK in hex
	c, _ := committedConfig(t)
	const hexKey = "00112233445566778899aabbccddeeff"
	if err := c.AddApiUser(&ApiUser{
		Username:   "tv-user",
		DeviceType: "Philips#TV",
		ClientKey:  hexKey,
	}); err != nil {
		t.Fatalf("AddApiUser: %v", err)
	}

	// When
	psk, ok := c.PSKForUser("tv-user")

	// Then: the DTLS handshake needs the raw bytes, not the hex text
	if !ok {
		t.Fatal("PSKForUser reported no key for a paired user")
	}
	want := []byte{0x00, 0x11, 0x22, 0x33, 0x44, 0x55, 0x66, 0x77,
		0x88, 0x99, 0xaa, 0xbb, 0xcc, 0xdd, 0xee, 0xff}
	if !bytes.Equal(psk, want) {
		t.Fatalf("psk = % x, want % x", psk, want)
	}
}

func TestPSKForUser_unknownUser(t *testing.T) {
	// Given: a config with one paired user
	c, _ := committedConfig(t)
	if err := c.AddApiUser(&ApiUser{Username: "known", ClientKey: "aabb"}); err != nil {
		t.Fatalf("AddApiUser: %v", err)
	}

	// When
	psk, ok := c.PSKForUser("stranger")

	// Then
	if ok || psk != nil {
		t.Fatalf("PSKForUser(stranger) = % x, %v, want nil, false", psk, ok)
	}
}

func TestPSKForUser_userPairedWithoutAClientKey(t *testing.T) {
	// Given: a client paired without generateclientkey — it can use the REST API
	// but cannot stream entertainment
	c, _ := committedConfig(t)
	if err := c.AddApiUser(&ApiUser{Username: "rest-only", DeviceType: "app#x"}); err != nil {
		t.Fatalf("AddApiUser: %v", err)
	}

	// When
	psk, ok := c.PSKForUser("rest-only")

	// Then: not an error, simply no PSK
	if ok || psk != nil {
		t.Fatalf("PSKForUser = % x, %v, want nil, false", psk, ok)
	}
	// The user itself is still known.
	if !c.HasApiUser("rest-only") {
		t.Error("a keyless user must still count as paired")
	}
}

func TestPSKForUser_nonHexClientKeyIsRejected(t *testing.T) {
	// Given: a corrupted clientkey (hand-edited config, truncated write)
	c, _ := committedConfig(t)
	for _, bad := range []string{"zzzz", "abc", "not-hex-at-all"} {
		if err := c.AddApiUser(&ApiUser{Username: "tv", ClientKey: bad}); err != nil {
			t.Fatalf("AddApiUser: %v", err)
		}

		// When
		psk, ok := c.PSKForUser("tv")

		// Then: refuse rather than hand a garbage key to the DTLS handshake
		if ok || psk != nil {
			t.Errorf("clientKey %q: got % x, %v, want nil, false", bad, psk, ok)
		}
	}
}

func TestPSKForUser_survivesAReload(t *testing.T) {
	// Given: a paired user written to disk
	c, path := committedConfig(t)
	if err := c.AddApiUser(&ApiUser{Username: "tv", ClientKey: "0f1e2d3c"}); err != nil {
		t.Fatalf("AddApiUser: %v", err)
	}

	// When: the daemon restarts and re-reads the file
	c2, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	psk, ok := c2.PSKForUser("tv")

	// Then
	if !ok {
		t.Fatal("the PSK did not survive the reload")
	}
	if !bytes.Equal(psk, []byte{0x0f, 0x1e, 0x2d, 0x3c}) {
		t.Fatalf("psk = % x", psk)
	}
}

func TestSave_writesModeAndLeavesNoTempFile(t *testing.T) {
	// Given: a committed config
	c, path := committedConfig(t)

	// When
	if err := c.AddApiUser(&ApiUser{Username: "tv", DeviceType: "Philips#TV"}); err != nil {
		t.Fatalf("AddApiUser: %v", err)
	}

	// Then: the config holds pairing secrets, so it must not be world-readable
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("config mode = %o, want 600", perm)
	}
	// The atomic write renames its temp file into place; nothing may linger.
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Errorf("a .tmp file was left behind (stat err = %v)", err)
	}
}

func TestSave_isSkippedEntirelyBeforeCommit(t *testing.T) {
	// Given: a fresh config mid-setup that has NOT been committed
	path := filepath.Join(t.TempDir(), "relume-tv.json")
	c, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	// When: setup-time mutations happen
	if err := c.AddApiUser(&ApiUser{Username: "tv", ClientKey: "aabb"}); err != nil {
		t.Fatalf("AddApiUser: %v", err)
	}
	if err := c.SaveEntConfigID("ent-1"); err != nil {
		t.Fatalf("SaveEntConfigID: %v", err)
	}

	// Then: nothing on disk yet, but the in-memory state is live
	if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
		t.Errorf("config written before Commit (stat err = %v)", statErr)
	}
	if !c.HasApiUser("tv") {
		t.Error("the in-memory user is missing")
	}
	if c.LoadEntConfigID() != "ent-1" {
		t.Errorf("ent config id = %q", c.LoadEntConfigID())
	}

	// And: Commit flushes everything at once
	if err := c.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	c2, err := Load(path)
	if err != nil {
		t.Fatalf("Load 2: %v", err)
	}
	if !c2.HasApiUser("tv") || c2.LoadEntConfigID() != "ent-1" {
		t.Error("Commit did not flush the pending setup state")
	}
}

func TestSave_createsTheParentDirectory(t *testing.T) {
	// Given: a config path inside a directory that does not exist yet — the
	// container's first start with an empty volume
	path := filepath.Join(t.TempDir(), "nested", "deeper", "relume-tv.json")
	c, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	// When
	if err := c.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	// Then
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("config not written into the created directory: %v", err)
	}
}

func TestSave_surfacesAWriteFailure(t *testing.T) {
	// Given: a committed config, then a DIRECTORY squatting on the temp-file path
	// the atomic write needs. Opening it for writing must fail.
	c, path := committedConfig(t)
	if err := os.Mkdir(path+".tmp", 0o755); err != nil {
		t.Fatalf("squat on the temp path: %v", err)
	}

	// When
	err := c.AddApiUser(&ApiUser{Username: "tv", DeviceType: "Philips#TV"})

	// Then: a failed persist must be reported, not silently swallowed — otherwise
	// the pairing looks successful but is lost on restart
	if err == nil {
		t.Fatal("expected the save to fail when the temp path is unwritable")
	}
}

func TestSave_failedRenameLeavesNoTempFile(t *testing.T) {
	// Given: a committed config whose final path is replaced by a non-empty
	// DIRECTORY, so the rename over it cannot succeed
	c, path := committedConfig(t)
	if err := os.Remove(path); err != nil {
		t.Fatalf("remove config: %v", err)
	}
	if err := os.Mkdir(path, 0o755); err != nil {
		t.Fatalf("mkdir over the config path: %v", err)
	}
	if err := os.WriteFile(filepath.Join(path, "occupant"), []byte("x"), 0o600); err != nil {
		t.Fatalf("fill the directory: %v", err)
	}

	// When
	err := c.AddApiUser(&ApiUser{Username: "tv"})

	// Then: the error surfaces AND the half-written temp file is cleaned up, so
	// a retry does not trip over its own garbage
	if err == nil {
		t.Fatal("expected the rename to fail")
	}
	if _, statErr := os.Stat(path + ".tmp"); !os.IsNotExist(statErr) {
		t.Errorf("a temp file was left behind after a failed rename (stat err = %v)", statErr)
	}
}

func TestWriteFileSync_writesExactBytesAt0600(t *testing.T) {
	// Given
	path := filepath.Join(t.TempDir(), "out.bin")
	data := []byte("relume-tv\x00binary\xffpayload")

	// When
	if err := writeFileSync(path, data); err != nil {
		t.Fatalf("writeFileSync: %v", err)
	}

	// Then
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Fatalf("round-trip mismatch: % x", got)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("mode = %o, want 600", perm)
	}
}

func TestWriteFileSync_truncatesAnExistingLongerFile(t *testing.T) {
	// Given: an existing file longer than the new contents. Without O_TRUNC the
	// tail of the old config would survive and corrupt the JSON.
	path := filepath.Join(t.TempDir(), "out.json")
	if err := os.WriteFile(path, []byte("aaaaaaaaaaaaaaaaaaaaaaaaaaaa"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// When
	if err := writeFileSync(path, []byte("short")); err != nil {
		t.Fatalf("writeFileSync: %v", err)
	}

	// Then
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(got) != "short" {
		t.Fatalf("got %q, want %q — the file was not truncated", got, "short")
	}
}

func TestWriteFileSync_reportsAnUnopenablePath(t *testing.T) {
	// Given: a path whose parent directory does not exist
	path := filepath.Join(t.TempDir(), "missing-dir", "out.bin")

	// When
	err := writeFileSync(path, []byte("x"))

	// Then
	if err == nil {
		t.Fatal("expected an error for an unopenable path")
	}
}

func TestFsyncDir_toleratesAMissingDirectory(t *testing.T) {
	// Given / When: the directory fsync is documented as best-effort, because some
	// platforms cannot sync a directory handle at all
	err := fsyncDir(filepath.Join(t.TempDir(), "does-not-exist"))

	// Then
	if err != nil {
		t.Fatalf("fsyncDir must stay best-effort, got %v", err)
	}
}

func TestGenerateSerial_is12HexCharsAndRandom(t *testing.T) {
	// When
	seen := map[string]bool{}
	for i := 0; i < 50; i++ {
		s, err := generateSerial()
		if err != nil {
			t.Fatalf("generateSerial: %v", err)
		}

		// Then: 6 bytes hex-encoded, which is what MAC()/BridgeID() slice up
		if len(s) != 12 {
			t.Fatalf("serial %q has length %d, want 12", s, len(s))
		}
		for _, r := range s {
			if !('0' <= r && r <= '9') && !('a' <= r && r <= 'f') {
				t.Fatalf("serial %q contains a non-lowercase-hex rune %q", s, r)
			}
		}
		seen[s] = true
	}
	if len(seen) < 45 {
		t.Fatalf("only %d distinct serials out of 50 — the source is not random", len(seen))
	}
}

func TestGeneratedSerialFeedsTheDerivedIdentifiers(t *testing.T) {
	// Given: a generated serial used as a real Identity
	s, err := generateSerial()
	if err != nil {
		t.Fatalf("generateSerial: %v", err)
	}
	id := Identity{Serial: s}

	// Then: the SSDP/UPnP identifiers must all be well-formed for any serial
	if len(id.MAC()) != 17 {
		t.Errorf("MAC = %q", id.MAC())
	}
	if len(id.BridgeID()) != 16 {
		t.Errorf("BridgeID = %q", id.BridgeID())
	}
	if len(id.UUID()) != 36 {
		t.Errorf("UUID = %q", id.UUID())
	}
}
