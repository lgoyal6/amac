// Package machine reads what this Mac has left.
//
// amac already had two numbers for this, swap and disk, scraped out of the
// tmux reaper's log line every thirty minutes. That was enough to raise a
// warning and not enough to answer the question the warning provokes: what is
// actually using it. A percentage with no breakdown tells you to worry without
// telling you what to close.
//
// So this reads the live figures instead of a log, at the moment somebody
// looks. Nothing is cached beyond a short window: a capacity reading is only
// interesting because it changes, and a stale one is the failure mode the
// scraped version already had.
package machine

import (
	"bufio"
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Stats is one reading of the three things that run out.
type Stats struct {
	At     time.Time `json:"at"`
	Disk   Disk      `json:"disk"`
	Memory Memory    `json:"memory"`
	Swap   Swap      `json:"swap"`
}

type Disk struct {
	Mount   string `json:"mount"`
	Total   int64  `json:"total"`
	Used    int64  `json:"used"`
	Free    int64  `json:"free"`
	Percent int    `json:"percent"`
}

// Memory is the split Activity Monitor shows, because that is the one the
// person reading it can act on. "Used" alone cannot distinguish a machine
// swapping under real load from one that has simply filled its cache, which on
// macOS is the normal and healthy state.
type Memory struct {
	Total int64 `json:"total"`
	// Wired cannot be paged out at all: the kernel and its drivers.
	Wired int64 `json:"wired"`
	// App is what the programs asked for. This is the number that goes down
	// when you quit something.
	App int64 `json:"app"`
	// Compressed is memory the kernel squeezed rather than swapped. Its
	// presence is the warning sign, because compression is what happens just
	// before swapping starts.
	Compressed int64 `json:"compressed"`
	// Available is free plus everything reclaimable, chiefly the file cache.
	// Counting cache as "used" is what makes naive memory graphs frightening
	// and useless on macOS.
	Available int64 `json:"available"`
	Percent   int   `json:"percent"`
}

type Swap struct {
	Total   int64 `json:"total"`
	Used    int64 `json:"used"`
	Free    int64 `json:"free"`
	Percent int   `json:"percent"`
}

// dataVolume is where a modern macOS keeps everything that is not the sealed
// system snapshot. Reading "/" instead reports the read-only system volume,
// which is always about 38% full and never the answer to "am I out of space".
const dataVolume = "/System/Volumes/Data"

// cache keeps one reading for a moment. The dashboard polls, and three forks
// per poll for numbers that move on the order of seconds is a cost with no
// corresponding gain in truth.
var cache struct {
	mu   sync.Mutex
	at   time.Time
	last Stats
}

const freshFor = 5 * time.Second

// Read returns the current reading, or a very recent one.
func Read(ctx context.Context) (Stats, error) {
	cache.mu.Lock()
	defer cache.mu.Unlock()
	if !cache.at.IsZero() && time.Since(cache.at) < freshFor {
		return cache.last, nil
	}

	s := Stats{At: time.Now()}
	disk, err := readDisk(dataVolume)
	if err != nil {
		return Stats{}, err
	}
	s.Disk = disk

	mem, err := readMemory(ctx)
	if err != nil {
		return Stats{}, err
	}
	s.Memory = mem

	if sw, err := readSwap(ctx); err == nil {
		s.Swap = sw
	}
	// A machine with swap disabled is not an error, and it has no swap row.

	cache.at, cache.last = time.Now(), s
	return s, nil
}

// readDisk uses statfs rather than parsing df, because the numbers are the
// point and df's output is a table meant for people.
func readDisk(mount string) (Disk, error) {
	var fs syscall.Statfs_t
	if err := syscall.Statfs(mount, &fs); err != nil {
		// A Mac before the volume split, or a test machine. "/" is then the
		// whole story rather than a third of it.
		mount = "/"
		if err := syscall.Statfs(mount, &fs); err != nil {
			return Disk{}, fmt.Errorf("statfs %s: %w", mount, err)
		}
	}
	block := int64(fs.Bsize)
	total := int64(fs.Blocks) * block
	// Free to this user, not free to root: the reserve is not space you have.
	free := int64(fs.Bavail) * block
	used := total - free
	return Disk{Mount: mount, Total: total, Used: used, Free: free, Percent: percent(used, total)}, nil
}

// readMemory parses vm_stat, which is the only interface to these counters:
// they come from host_statistics64 and no sysctl exposes them.
func readMemory(ctx context.Context) (Memory, error) {
	out, err := exec.CommandContext(ctx, "vm_stat").Output()
	if err != nil {
		return Memory{}, fmt.Errorf("vm_stat: %w", err)
	}
	pageSize, pages := parseVMStat(string(out))
	if pageSize == 0 {
		return Memory{}, fmt.Errorf("vm_stat: no page size in output")
	}

	total, err := sysctlInt(ctx, "hw.memsize")
	if err != nil {
		return Memory{}, err
	}

	m := Memory{
		Total: total,
		Wired: pages["wired down"] * pageSize,
		// Anonymous is what the programs hold; purgeable is the part of it
		// they have already told the kernel it may throw away.
		App:        (pages["anonymous"] - pages["purgeable"]) * pageSize,
		Compressed: pages["occupied by compressor"] * pageSize,
	}
	if m.App < 0 {
		m.App = 0
	}
	m.Available = total - m.Wired - m.App - m.Compressed
	if m.Available < 0 {
		m.Available = 0
	}
	m.Percent = percent(total-m.Available, total)
	return m, nil
}

// parseVMStat turns vm_stat's prose into counters keyed by the words that
// identify them, so a renamed or reordered line is a missing key rather than a
// wrong number read from the wrong row.
func parseVMStat(out string) (pageSize int64, pages map[string]int64) {
	pages = map[string]int64{}
	sc := bufio.NewScanner(strings.NewReader(out))
	for sc.Scan() {
		line := sc.Text()
		if pageSize == 0 && strings.Contains(line, "page size of") {
			if _, after, ok := strings.Cut(line, "page size of "); ok {
				pageSize, _ = strconv.ParseInt(strings.Fields(after)[0], 10, 64)
			}
			continue
		}
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		n, err := strconv.ParseInt(strings.TrimSuffix(strings.TrimSpace(value), "."), 10, 64)
		if err != nil {
			continue
		}
		key = strings.TrimSpace(strings.TrimPrefix(key, "Pages"))
		key = strings.TrimSuffix(strings.TrimSpace(key), " pages")
		pages[strings.ToLower(key)] = n
	}
	return pageSize, pages
}

// readSwap parses vm.swapusage, whose value is a sentence rather than a number:
//
//	total = 24576.00M  used = 23565.88M  free = 1010.12M  (encrypted)
func readSwap(ctx context.Context) (Swap, error) {
	out, err := exec.CommandContext(ctx, "sysctl", "-n", "vm.swapusage").Output()
	if err != nil {
		return Swap{}, fmt.Errorf("sysctl vm.swapusage: %w", err)
	}
	s := Swap{}
	for _, field := range strings.Fields(string(out)) {
		name, value, ok := strings.Cut(field, "=")
		if !ok {
			continue
		}
		n := parseSize(value)
		switch name {
		case "total":
			s.Total = n
		case "used":
			s.Used = n
		case "free":
			s.Free = n
		}
	}
	// The fields arrive as "total" "=" "24576.00M" on some releases and as
	// "total=24576.00M" on others, so a run that found nothing tries again
	// against the whole string rather than reporting an empty swap.
	if s.Total == 0 {
		s = parseSwapSpaced(string(out))
	}
	s.Percent = percent(s.Used, s.Total)
	return s, nil
}

func parseSwapSpaced(out string) Swap {
	var s Swap
	fields := strings.Fields(out)
	for i, f := range fields {
		if f != "=" || i == 0 || i+1 >= len(fields) {
			continue
		}
		n := parseSize(fields[i+1])
		switch fields[i-1] {
		case "total":
			s.Total = n
		case "used":
			s.Used = n
		case "free":
			s.Free = n
		}
	}
	return s
}

// parseSize reads "23565.88M" as bytes. The suffix is what sysctl chose to
// print, so it is read rather than assumed.
func parseSize(v string) int64 {
	if v == "" {
		return 0
	}
	mult := int64(1)
	switch v[len(v)-1] {
	case 'K':
		mult, v = 1<<10, v[:len(v)-1]
	case 'M':
		mult, v = 1<<20, v[:len(v)-1]
	case 'G':
		mult, v = 1<<30, v[:len(v)-1]
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return 0
	}
	return int64(f * float64(mult))
}

func sysctlInt(ctx context.Context, name string) (int64, error) {
	out, err := exec.CommandContext(ctx, "sysctl", "-n", name).Output()
	if err != nil {
		return 0, fmt.Errorf("sysctl %s: %w", name, err)
	}
	return strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
}

func percent(part, whole int64) int {
	if whole <= 0 {
		return 0
	}
	return int(float64(part) / float64(whole) * 100)
}
