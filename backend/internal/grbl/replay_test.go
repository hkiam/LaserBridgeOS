package grbl

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Replaying a transcript is how a real conversation becomes a test.
//
// Everything else here is checked against a simulator, and a simulator is a
// statement of belief. The one that stood in for a controller in these tests
// appended the Ov: block to every single status report, which no GRBL does -
// and that single wrong belief is what hid a cutting machine being displayed
// as "Laser output: off" for as long as it was hidden.
//
// The format is the monitor port's, so capturing one is a matter of setting
// grbl.monitor_port and redirecting nc into a file. Only the controller's side
// is replayed; that is the direction the observer reads.
func replay(t *testing.T, name string) *Observer {
	t.Helper()
	file, err := os.Open(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()

	observer := NewObserver()
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "< ") {
			continue
		}
		observer.Write([]byte(strings.TrimPrefix(line, "< ") + "\r\n"))
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	return observer
}

func TestACutIsReadTheWayTheControllerReportsIt(t *testing.T) {
	observer := replay(t, "cutting-m4.transcript")
	machine := observer.Machine()

	// The transcript ends with M5 and a report carrying Ov: and no accessory,
	// which is the controller saying the output is off. That is the one place
	// in the whole conversation where "off" is a statement rather than a gap.
	if machine.Beam != BeamOff {
		t.Errorf("beam = %q at the end of the cut, want off", machine.Beam)
	}
	// Four reports out of thirty carried the block: the two forced by the
	// output changing, the one that opened the file, and the one where the
	// counter ran out mid-cut. If this number ever climbs towards the report
	// count, the transcript has stopped resembling a controller.
	if machine.BeamReports > machine.Reports {
		t.Fatalf("bookkeeping is impossible: %d of %d", machine.BeamReports, machine.Reports)
	}
	if machine.Reports < 25 {
		t.Fatalf("only %d status reports replayed; the transcript is not a cut", machine.Reports)
	}
}

func TestTheGapsBetweenBlocksAreNotReadAsTheBeamGoingOff(t *testing.T) {
	// The failure this exists for. During the cut, nineteen consecutive reports
	// carry FS:800,450 and no Ov: at all. A reading that took those as "nothing
	// is on" would have shown a cutting machine as off - which is exactly what
	// was seen on a real one.
	file, err := os.Open(filepath.Join("testdata", "cutting-m4.transcript"))
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()

	observer := NewObserver()
	scanner := bufio.NewScanner(file)
	sawCuttingWithoutBlock := 0
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "< ") {
			continue
		}
		text := strings.TrimPrefix(line, "< ")
		observer.Write([]byte(text + "\r\n"))
		machine := observer.Machine()
		if machine.State != StateRun || machine.Spindle == 0 {
			continue
		}
		if !strings.Contains(text, "Ov:") {
			sawCuttingWithoutBlock++
			if machine.Beam != BeamOn {
				t.Fatalf("cutting at S%v and the reading says %q: %s", machine.Spindle, machine.Beam, text)
			}
		}
	}
	if sawCuttingWithoutBlock < 10 {
		t.Fatalf("only %d cutting reports without the block; the transcript is too talkative to prove anything",
			sawCuttingWithoutBlock)
	}
}
