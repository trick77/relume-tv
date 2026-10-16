# AGENTS.md — relume-tv

Emulates a gen-2 Hue Bridge (BSB002) toward a Philips Ambilight TV; proxies to a real Hue
Bridge Pro (BSB003) via CLIP v2. Go module in `backend/`, web UI (vanilla, no node) in `ui/`.
All repo content is English.

## build/test
- `make build` / `make test` from the root: both copy `ui/` into `backend/web/dist` first (go:embed
  can't reach `../ui`). Bare `go test` in `backend/` fails the asset tests on purpose.
- macOS mDNSResponder owns 5353 → test discovery on Linux (the NAS). Dual-homed host = bad test env.

## invariants the TV enforces (rejects otherwise)
- modelid `BSB002` in /config, mDNS TXT, SSDP and description.xml `<modelNumber>` (real bridges send
  `929000226503`; the working ha-hue-entertainment emulator sends `BSB002`).
- description.xml served as `Content-Type: text/xml`, not application/xml.
- bridgeid = upper(serial[:6] + "FFFE" + serial[6:]), serial 12 hex; UUID `2f402f80-da50-11e1-9b23-<serial>`.
  UUID identical across SSDP USN and description.xml UDN; bridgeid across SSDP header, mDNS TXT, /config.
- mDNS instance name exactly `Philips Hue - XXXXXX` (last 6 of bridgeid); `DIYHue-XXXXXX` is NOT found.
  It goes into hashicorp/mdns ESCAPED (`Philips\ Hue\ -\ …`): the zone compares a queried name byte
  for byte as miekg/dns unpacks it, unescaped it never answers a direct SRV/TXT query.
- mDNS: register exactly once, never re-announce, never Shutdown. zeroconf's goodbye (TTL 0) evicted
  relume-tv from the TV's cache = the original "never listed" bug; hashicorp/mdns sends none, keep it.
- SSDP: answer only ssdp:all or our own STs. The TV searches MediaServer/DIAL for itself; a Hue reply
  to those lands in an unrelated UPnP stack.
- pairing auto-accepted ONLY for the TV (`-tv-ip`, Philips/TV User-Agent, TP Vision platform code like
  `TPM191E Build/`); anyone else gets error 101. The TV hardcodes REST to :80.
- CLIP v1 `[{"error":{…}}]` is the Hue protocol, not kit's error body: keep.

## discovery (the hard part)
- Measured (65OLED806/Android 11): the TV actively queries `_hue._tcp`, then fetches `/description.xml`.
  NO Hue SSDP M-SEARCH, NO cloud. mDNS is the PRIMARY path, SSDP secondary.
- CONFIRMED: with a powered-on Bridge Pro on the LAN (it announces `_hue._tcp` as BSB003) the TV never
  lists relume-tv; Pro OFF → listed and paired instantly. THE open product problem, unsolved.
- hashicorp/mdns (replaced grandcat/zeroconf, #157, works behind an avahi reflector): the server answers
  unicast to the querier only, never announces, TTL 120 — fine because the TV queries actively.
  Discover sets the QU bit and the advertise iface (the lib joins the group on the default iface only).
  Its follow-up query for an incomplete instance is typed PTR; if a reflector still drops bridges, the
  fix is a small miekg/dns resolver doing SRV+TXT+A per instance (proven over the same reflector).

## Bridge Pro (BSB003)
- HTTPS:443 and CLIP v2 only. Self-signed Signify cert → pin the leaf SHA-256, never trust the chain.
- pair: POST /api {devicetype,generateclientkey:true}; button = brief TAP; error 101 = not pressed.
  Pairing is automatic in `serve`; re-discovery after an IP change never re-pairs (keys persist).
- PUT = 207 with per-attribute `errors[]` even when HTTP-ok: inspect errors[].
- CT-only lights reject `color.xy` → only color-capable lights are offered; v1 ids = sorted-UUID
  order over the kept lights.
- `translate.StateV1ToV2` must accept `xy` as `[]any` AND `[]float64`/`[]float32` (entertainment
  decode path), or lights stick on their last colour (the "stuck red" bug).
- REST forwarding alone overflows the Pro's command queue (503) → entertainment mode (DTLS from the TV,
  DTLS to the Pro) is the default; DTLS and REST are mutually exclusive, never both. A restart
  mid-stream orphans the TV's session: toggle Ambilight (not Ambilight+Hue) on the TV.

## deployment
- Same L2 as the TV → `network_mode: host`. Image runs AS ROOT (ship.yaml allow): :80 and root-owned
  /data volumes of existing installs; `:nonroot` breaks every install.
- Web UI :33100, no auth, read-only. State `/data/relume-tv.json` holds the Pro appKey/clientkey and
  TV tokens: gitignored, never commit.
- Gate: `ship check` ([ship](https://github.com/trick77/ship), `ship.yaml`; `ship` is the `ship()` from ship's README). `ship hooks` once per clone: pre-push runs it. CI on every pull request (by hand: `gh workflow run CI --ref <branch>`); release by hand only: `gh workflow run Release -f
  bump=patch` (plain tag, amd64+arm64 image to ghcr.io).
- Commit subjects + PR titles: Go style `area: what` (`setup: …`, `clipv1: …`), gated by ship; release notes list them by area.
- Keep x/net, x/sys, x/crypto upgraded: go 1.26 with an old x/net failed to link (`syscall.recvmsg`).

## status
Verified end to end on a real 65OLED806 + Bridge Pro (2026-06-16). Next: M4 phase D, config
persistence + activation lifecycle.
