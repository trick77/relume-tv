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
	if err := c.AddAPIUser(&APIUser{
		Username:   "tv-user",
		DeviceType: "Philips#TV",
		ClientKey:  hexKey,
	}); err != nil {
		t.Fatalf("AddAPIUser: %v", err)
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
	if err := c.AddAPIUser(&APIUser{Username: "known", ClientKey: "aabb"}); err != nil {
		t.Fatalf("AddAPIUser: %v", err)
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
	if err := c.AddAPIUser(&APIUser{Username: "rest-only", DeviceType: "app#x"}); err != nil {
		t.Fatalf("AddAPIUser: %v", err)
	}

	// When
	psk, ok := c.PSKForUser("rest-only")

	// Then: not an error, simply no PSK
	if ok || psk != nil {
		t.Fatalf("PSKForUser = % x, %v, want nil, false", psk, ok)
	}
	// The user itself is still known.
	if !c.HasAPIUser("rest-only") {
		t.Error("a keyless user must still count as paired")
	}
}

func TestPSKForUser_nonHexClientKeyIsRejected(t *testing.T) {
	// Given: a corrupted clientkey (hand-edited config, truncated write)
	c, _ := committedConfig(t)
	for _, bad := range []string{"zzzz", "abc", "not-hex-at-all"} {
		if err := c.AddAPIUser(&APIUser{Username: "tv", ClientKey: bad}); err != nil {
			t.Fatalf("AddAPIUser: %v", err)
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
	if err := c.AddAPIUser(&APIUser{Username: "tv", ClientKey: "0f1e2d3c"}); err != nil {
		t.Fatalf("AddAPIUser: %v", err)
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
	if err := c.AddAPIUser(&APIUser{Username: "tv", DeviceType: "Philips#TV"}); err != nil {
		t.Fatalf("AddAPIUser: %v", err)
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
	if left := tempFiles(t, path); len(left) > 0 {
		t.Errorf("temp files left behind: %v", left)
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
	if err := c.AddAPIUser(&APIUser{Username: "tv", ClientKey: "aabb"}); err != nil {
		t.Fatalf("AddAPIUser: %v", err)
	}
	if err := c.SaveEntConfigID("ent-1"); err != nil {
		t.Fatalf("SaveEntConfigID: %v", err)
	}

	// Then: nothing on disk yet, but the in-memory state is live
	if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
		t.Errorf("config written before Commit (stat err = %v)", statErr)
	}
	if !c.HasAPIUser("tv") {
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
	if !c2.HasAPIUser("tv") || c2.LoadEntConfigID() != "ent-1" {
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

// tempFiles lists what the atomic write may leave beside path: fsx's
// ".<name>.tmp-*" and the legacy "<name>.tmp".
func tempFiles(t *testing.T, path string) []string {
	t.Helper()
	dir, base := filepath.Split(path)
	matches, err := filepath.Glob(filepath.Join(dir, "."+base+".tmp-*"))
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	if _, err := os.Stat(path + ".tmp"); err == nil {
		matches = append(matches, path+".tmp")
	}
	return matches
}

func TestSave_surfacesAWriteFailure(t *testing.T) {
	// Given: a committed config in a directory that no longer accepts new files,
	// so the atomic write cannot create its temp file
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	c, path := committedConfig(t)
	dir := filepath.Dir(path)
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	// When
	err := c.AddAPIUser(&APIUser{Username: "tv", DeviceType: "Philips#TV"})

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
	err := c.AddAPIUser(&APIUser{Username: "tv"})

	// Then: the error surfaces AND the half-written temp file is cleaned up, so
	// a retry does not trip over its own garbage
	if err == nil {
		t.Fatal("expected the rename to fail")
	}
	if left := tempFiles(t, path); len(left) > 0 {
		t.Errorf("temp files left behind after a failed rename: %v", left)
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
			isDigit := '0' <= r && r <= '9'
			isLowerHex := 'a' <= r && r <= 'f'
			if !isDigit && !isLowerHex {
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
