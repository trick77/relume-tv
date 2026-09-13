package ssdp

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/trick77/relume-tv/internal/config"
)

func testResponder() *Responder {
	return New(config.Identity{Serial: "2c4d54ea2832"}, "192.0.2.10", 80, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func TestNotifyMessages_matchHueBridgeShape(t *testing.T) {
	// Given
	r := testResponder()

	// When
	msgs := r.notifyMessages()

	// Then
	if len(msgs) != 3 {
		t.Fatalf("notify message count = %d, expected 3", len(msgs))
	}
	for _, want := range []string{
		"NT: upnp:rootdevice\r\n",
		"NT: uuid:2f402f80-da50-11e1-9b23-2c4d54ea2832\r\n",
		"NT: urn:schemas-upnp-org:device:basic:1\r\n",
		"USN: uuid:2f402f80-da50-11e1-9b23-2c4d54ea2832::urn:schemas-upnp-org:device:basic:1\r\n",
	} {
		found := false
		for _, msg := range msgs {
			if strings.Contains(msg, want) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("notify messages do not contain %q:\n%s", want, strings.Join(msgs, "\n---\n"))
		}
	}
	for _, msg := range msgs {
		for _, want := range []string{
			"NOTIFY * HTTP/1.1\r\n",
			"HOST: 239.255.255.250:1900\r\n",
			"LOCATION: http://192.0.2.10:80/description.xml\r\n",
			"SERVER: Linux/3.14.0 UPnP/1.0 IpBridge/1.20.0\r\n",
			"NTS: ssdp:alive\r\n",
			"hue-bridgeid: 2C4D54FFFEEA2832\r\n",
		} {
			if !strings.Contains(msg, want) {
				t.Errorf("notify message missing %q:\n%s", want, msg)
			}
		}
	}
}

func TestSearchResponses_useDefaultServerHeaderAndBaseVariants(t *testing.T) {
	// Given
	r := testResponder()

	// When
	msgs := r.searchResponses("ssdp:all")

	// Then: exactly the 3 base variants, default server header, plain location
	if len(msgs) != 3 {
		t.Fatalf("search response count = %d, expected 3", len(msgs))
	}
	for _, msg := range msgs {
		if !strings.Contains(msg, "SERVER: Linux/3.14.0 UPnP/1.0 IpBridge/1.20.0\r\n") {
			t.Errorf("search response missing default server header:\n%s", msg)
		}
		if !strings.Contains(msg, "LOCATION: http://192.0.2.10:80/description.xml\r\n") {
			t.Errorf("search response missing plain location:\n%s", msg)
		}
		if !strings.Contains(msg, "CACHE-CONTROL: max-age=100\r\n") {
			t.Errorf("search response missing max-age=100:\n%s", msg)
		}
	}
	joined := strings.Join(msgs, "\n---\n")
	for _, want := range []string{
		"ST: uuid:2f402f80-da50-11e1-9b23-2c4d54ea2832\r\n",
		"USN: uuid:2f402f80-da50-11e1-9b23-2c4d54ea2832::upnp:rootdevice\r\n",
		"USN: uuid:2f402f80-da50-11e1-9b23-2c4d54ea2832\r\n",
		"USN: uuid:2f402f80-da50-11e1-9b23-2c4d54ea2832::urn:schemas-upnp-org:device:basic:1\r\n",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("search responses missing %q:\n%s", want, joined)
		}
	}
}

func TestSearchResponses_answerOnlyOwnSearchTargets(t *testing.T) {
	r := testResponder()
	cases := []struct {
		st   string
		want int
	}{
		{"ssdp:all", 3},
		{"upnp:rootdevice", 1},
		{"uuid:2f402f80-da50-11e1-9b23-2c4d54ea2832", 1},
		{"urn:schemas-upnp-org:device:basic:1", 1},
		{"urn:schemas-upnp-org:device:MediaServer:1", 0}, // the TV's own DLNA search
		{"urn:dial-multiscreen-org:service:dial:1", 0},
		{"uuid:some-other-device", 0},
		{"", 0},
	}
	for _, c := range cases {
		msgs := r.searchResponses(c.st)
		if len(msgs) != c.want {
			t.Errorf("ST %q: %d responses, want %d", c.st, len(msgs), c.want)
		}
		if c.want == 1 && !strings.Contains(msgs[0], "ST: "+c.st+"\r\n") {
			t.Errorf("ST %q: response carries a different ST:\n%s", c.st, msgs[0])
		}
	}
}

func TestRunBurst_sendsImmediatelyAndOnIntervalUntilDuration(t *testing.T) {
	// Given
	ctx := context.Background()
	count := 0

	// When
	runBurst(ctx, 10*time.Millisecond, 35*time.Millisecond, func() {
		count++
	})

	// Then: one immediate send, then roughly at 10ms/20ms/30ms.
	if count < 4 {
		t.Fatalf("burst count = %d, expected at least 4", count)
	}
	if count > 5 {
		t.Fatalf("burst count = %d, expected no more than 5", count)
	}
}
