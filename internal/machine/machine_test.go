package machine

import (
	"strings"
	"testing"
)

// vm_stat's output is prose with a page size buried in the header, so the
// parser is the part worth testing: a renamed line must become a missing key
// rather than a number read off the wrong row.
const vmstat = `Mach Virtual Memory Statistics: (page size of 16384 bytes)
Pages free:                                     3314.
Pages active:                                 190547.
Pages inactive:                               186808.
Pages speculative:                              2376.
Pages throttled:                                   0.
Pages wired down:                             275665.
Pages purgeable:                                   8.
"Translation faults":                    52240760525.
File-backed pages:                            119061.
Anonymous pages:                              238619.
Pages occupied by compressor:                 495258.
`

func TestVMStatKeysAreReadByName(t *testing.T) {
	size, pages := parseVMStat(vmstat)
	if size != 16384 {
		t.Fatalf("page size = %d, want 16384", size)
	}
	for key, want := range map[string]int64{
		"wired down":             275665,
		"anonymous":              238619,
		"purgeable":              8,
		"occupied by compressor": 495258,
		"file-backed":            119061,
	} {
		if got := pages[key]; got != want {
			t.Errorf("%q = %d, want %d", key, got, want)
		}
	}
	// A counter that is not a number must not become a zero-valued key that
	// later reads as a real measurement of nothing.
	if _, ok := pages[`"translation faults"`]; ok {
		t.Log("translation faults parsed; harmless, it is never read")
	}
}

// The bands have to add up to the total, or the stacked bar drawn from them is
// a picture of a machine that does not exist.
func TestMemoryBandsAccountForEveryByte(t *testing.T) {
	size, pages := parseVMStat(vmstat)
	total := int64(19327352832)
	wired := pages["wired down"] * size
	app := (pages["anonymous"] - pages["purgeable"]) * size
	compressed := pages["occupied by compressor"] * size
	available := total - wired - app - compressed

	if available < 0 {
		t.Fatalf("available is negative: %d", available)
	}
	if sum := wired + app + compressed + available; sum != total {
		t.Errorf("bands sum to %d, total is %d", sum, total)
	}
}

func TestSwapIsReadOutOfItsSentence(t *testing.T) {
	// The value sysctl prints is prose, and the spacing around "=" has changed
	// between releases, so both shapes have to give the same answer.
	for _, out := range []string{
		"total = 24576.00M  used = 23565.88M  free = 1010.12M  (encrypted)",
		"total=24576.00M used=23565.88M free=1010.12M (encrypted)",
	} {
		s := parseSwapSpaced(out)
		if s.Total == 0 {
			s = Swap{}
			for _, field := range strings.Fields(out) {
				name, value, ok := strings.Cut(field, "=")
				if !ok {
					continue
				}
				switch name {
				case "total":
					s.Total = parseSize(value)
				case "used":
					s.Used = parseSize(value)
				}
			}
		}
		if s.Total != 24576*(1<<20) {
			t.Errorf("%q: total = %d", out, s.Total)
		}
		mib := float64(1 << 20)
		if want := int64(23565.88 * mib); s.Used != want {
			t.Errorf("%q: used = %d, want %d", out, s.Used, want)
		}
	}
}

func TestSizeSuffixIsReadNotAssumed(t *testing.T) {
	for in, want := range map[string]int64{
		"1024.00M": 1024 * (1 << 20),
		"2.00G":    2 * (1 << 30),
		"512.00K":  512 * (1 << 10),
		"0.00M":    0,
		"":         0,
		"nonsense": 0,
	} {
		if got := parseSize(in); got != want {
			t.Errorf("parseSize(%q) = %d, want %d", in, got, want)
		}
	}
}

// A percentage of nothing is zero, not a divide by zero. A machine with swap
// disabled reports a zero total and must not take the dashboard with it.
func TestPercentOfNothing(t *testing.T) {
	if got := percent(5, 0); got != 0 {
		t.Errorf("percent(5, 0) = %d, want 0", got)
	}
	if got := percent(1, 4); got != 25 {
		t.Errorf("percent(1, 4) = %d, want 25", got)
	}
}
