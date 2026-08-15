package grbl

import (
	"strings"
	"testing"
)

// FuzzParse feeds the parser whatever comes.
//
// Its input is a serial line: bytes from a device, at a baud rate that may be
// wrong, over a cable that may be picking up interference from the machine it
// is attached to. Half a status report, a line of noise, a report from
// firmware nobody here has heard of - none of those may panic, and none may
// come out claiming to be something they are not.
func FuzzParse(f *testing.F) {
	for _, seed := range []string{
		"<Idle|MPos:0.000,0.000,0.000|FS:0,0>",
		"<Run|MPos:1.5,-2.25,0|FS:900,255|Ov:100,100,100|A:S>",
		"<Hold:0|WPos:10.000,20.000>",
		"ok", "error:9", "ALARM:1", "[MSG:Caution: Unlocked]",
		"Grbl 1.1f ['$' for help]", "$32=1", "$$", "",
		"<", ">", "<>", "<|||>", "<Idle|MPos:>", "<Idle|MPos:,,>",
		"<Idle|FS:notanumber>", "error:", "error:notanumber",
		"<Idle|Ov:>", "<Idle|A:>", strings.Repeat("x", 4096),
		"\x00\xff\xfe", "<\x00Idle>",
	} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, line string) {
		report := Parse(line)

		// A report that claims to be a status report has to have come from
		// something shaped like one, because everything downstream - including
		// the decision to stop a machine - trusts that.
		if report.Kind == "status" {
			trimmed := strings.TrimSpace(line)
			if !strings.HasPrefix(trimmed, "<") || !strings.HasSuffix(trimmed, ">") {
				t.Fatalf("Parse(%q) claims to be a status report", line)
			}
		}
		if report.HasPosition && report.Kind != "status" {
			t.Fatalf("Parse(%q) reports a position without being a status report", line)
		}
		// The accessory field is the evidence a laser is switched off on. It
		// may only come from a report that carried one.
		if report.Accessory != "" && !strings.Contains(line, "A:") {
			t.Fatalf("Parse(%q) invented an accessory state %q", line, report.Accessory)
		}
		if report.HasOverrides && !strings.Contains(line, "Ov:") {
			t.Fatalf("Parse(%q) invented an overrides field", line)
		}
		if report.Setting != "" && !strings.HasPrefix(strings.TrimSpace(line), "$") {
			t.Fatalf("Parse(%q) invented a setting %q", line, report.Setting)
		}
	})
}

// FuzzObserver feeds the same noise through the assembler, in arbitrary
// chunks, because that is how a serial port delivers it.
func FuzzObserver(f *testing.F) {
	f.Add("<Idle|MPos:0.000,0.000,0.000>\r\nok\r\n", 7)
	f.Add("Grbl 1.1f ['$' for help]\r\n$32=0\r\n", 3)
	f.Add(strings.Repeat("noise", 500), 13)

	f.Fuzz(func(t *testing.T, stream string, chunk int) {
		if chunk <= 0 {
			chunk = 1
		}
		observer := NewObserver()
		for i := 0; i < len(stream); i += chunk {
			end := i + chunk
			if end > len(stream) {
				end = len(stream)
			}
			observer.Write([]byte(stream[i:end]))
		}
		// Whatever arrived, the buffer it is assembled in stays bounded: this
		// runs on an appliance with 512 MB and a controller at the wrong baud
		// rate produces a stream with no line endings in it at all.
		observer.mu.Lock()
		length := observer.line.Len()
		observer.mu.Unlock()
		if length > maxLine {
			t.Fatalf("assembling %d bytes left %d buffered", len(stream), length)
		}
	})
}
