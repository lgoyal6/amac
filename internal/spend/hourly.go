package spend

// The snapshot is a per-day figure. An hour-by-hour view exists nowhere but in
// the transcripts themselves, so this is the one place the package reads the
// session logs instead of looseapi's projection of them. It deliberately stops
// at tokens: pricing them would be the second cost calculation the package
// comment warns against, and amac holds no per-model rate table to do it with.
// The table is a parameter so that the day one exists the dollars column fills
// in without this changing shape; until then it is nil and the column is null.

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Rate is what one model costs in USD per million tokens of each kind.
type Rate struct {
	Input, Output, CacheRead, CacheWrite float64
}

// HourBucket is one local hour of today. AgentUSD is nil when nothing in the
// hour could be priced; Tokens counts every kind of token together.
type HourBucket struct {
	Hour     int      `json:"hour"`
	AgentUSD *float64 `json:"agentUsd"`
	Tokens   int64    `json:"tokens"`
}

type Today struct {
	Date    string       `json:"date"`
	Buckets []HourBucket `json:"buckets"`
	Source  string       `json:"source"`
	Warning string       `json:"warning,omitempty"`
}

// TranscriptsDir is where Claude Code writes one session log per conversation.
func TranscriptsDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".claude", "projects")
}

// Hourly buckets today's assistant messages by the local hour they arrived.
//
// Every .jsonl under dir modified today is read, subagent logs included, which
// is the same set looseapi's daily figure is built from. Only the tail of each
// file is scanned: a session that has run for a week has a week of lines
// before today's, and the file is append-only and chronological, so a binary
// search on the timestamps finds where today starts without reading the rest.
//
// One API response is written as one line per content block, each carrying
// the usage so far, so a message is counted once, at the largest usage any of
// its lines reports, whichever file or order it turned up in. looseapi counts
// every line, which is why a day total from here comes out below the
// snapshot's.
func Hourly(dir string, now time.Time, rates map[string]Rate) Today {
	start := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	end := start.AddDate(0, 0, 1)

	out := Today{Date: start.Format("2006-01-02"), Buckets: make([]HourBucket, 24)}
	for h := range out.Buckets {
		out.Buckets[h].Hour = h
	}
	out.Source = "tokens only, no rate table in amac"
	if len(rates) > 0 {
		out.Source = "tokens from transcripts, priced from amac's rate table"
	}

	type message struct {
		at    time.Time
		model string
		usage tokenUsage
	}
	var files, unreadable int
	byID := map[string]message{}
	var unkeyed []message
	_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".jsonl") {
			return nil
		}
		info, err := d.Info()
		if err != nil || info.ModTime().Before(start) {
			return nil
		}
		files++
		err = scanTranscript(path, start, end, func(l transcriptLine, at time.Time) {
			m := message{at: at, model: l.Message.Model, usage: *l.Message.Usage}
			key := l.Message.ID
			if key == "" {
				key = l.RequestID
			}
			if key == "" {
				unkeyed = append(unkeyed, m)
				return
			}
			if prev, ok := byID[key]; !ok || m.usage.total() > prev.usage.total() {
				byID[key] = m
			}
		})
		if err != nil {
			unreadable++
		}
		return nil
	})

	var priced [24]bool
	var dollars [24]float64
	unpriced := map[string]bool{}
	messages := unkeyed
	for _, m := range byID {
		messages = append(messages, m)
	}
	for _, m := range messages {
		h := m.at.In(now.Location()).Hour()
		out.Buckets[h].Tokens += m.usage.total()
		if r, ok := rates[m.model]; ok {
			dollars[h] += r.usd(m.usage)
			priced[h] = true
		} else if len(rates) > 0 {
			unpriced[m.model] = true
		}
	}
	for h := range out.Buckets {
		if priced[h] {
			v := dollars[h]
			out.Buckets[h].AgentUSD = &v
		}
	}

	var warnings []string
	if files == 0 {
		warnings = append(warnings, fmt.Sprintf("no Claude Code transcripts modified today under %s", dir))
	}
	if unreadable > 0 {
		warnings = append(warnings, fmt.Sprintf("%d transcripts could not be read", unreadable))
	}
	if len(unpriced) > 0 {
		names := make([]string, 0, len(unpriced))
		for m := range unpriced {
			names = append(names, m)
		}
		sort.Strings(names)
		warnings = append(warnings, "no rate for "+strings.Join(names, ", ")+"; those tokens are counted but not priced")
	}
	out.Warning = strings.Join(warnings, "; ")
	return out
}

type transcriptLine struct {
	Type      string `json:"type"`
	Timestamp string `json:"timestamp"`
	RequestID string `json:"requestId"`
	Message   struct {
		ID    string      `json:"id"`
		Model string      `json:"model"`
		Usage *tokenUsage `json:"usage"`
	} `json:"message"`
}

type tokenUsage struct {
	Input      int64 `json:"input_tokens"`
	Output     int64 `json:"output_tokens"`
	CacheRead  int64 `json:"cache_read_input_tokens"`
	CacheWrite int64 `json:"cache_creation_input_tokens"`
}

func (u tokenUsage) total() int64 { return u.Input + u.Output + u.CacheRead + u.CacheWrite }

func (r Rate) usd(u tokenUsage) float64 {
	return (float64(u.Input)*r.Input + float64(u.Output)*r.Output +
		float64(u.CacheRead)*r.CacheRead + float64(u.CacheWrite)*r.CacheWrite) / 1e6
}

// scanTranscript calls visit for every assistant message stamped inside
// [start, end), reading from the first line that could be one and never
// holding more than a line in memory.
func scanTranscript(path string, start, end time.Time, visit func(transcriptLine, time.Time)) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	from := tailFrom(f, info.Size(), start)
	r := bufio.NewReaderSize(io.NewSectionReader(f, from, info.Size()-from), 1<<20)
	for {
		line, err := r.ReadBytes('\n')
		if bytes.Contains(line, []byte(`"usage"`)) {
			var l transcriptLine
			if json.Unmarshal(line, &l) == nil && l.Type == "assistant" && l.Message.Usage != nil {
				if at, perr := time.Parse(time.RFC3339Nano, l.Timestamp); perr == nil && !at.Before(start) && at.Before(end) {
					visit(l, at)
				}
			}
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

var stampKey = []byte(`"timestamp":"`)

// stampOf pulls the timestamp out of one line without decoding the rest of
// it, which can be half a megabyte of tool output.
func stampOf(line []byte) (time.Time, bool) {
	i := bytes.Index(line, stampKey)
	if i < 0 {
		return time.Time{}, false
	}
	rest := line[i+len(stampKey):]
	j := bytes.IndexByte(rest, '"')
	if j < 0 {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339Nano, string(rest[:j]))
	return t, err == nil
}

// tailFrom returns a line boundary at or before the first line stamped at or
// after t. Lines are chronological, so this is a binary search over byte
// offsets: a probe lands mid-line, skips to the next boundary, reads one
// stamped line and moves lo or hi by what it finds. Lines with no timestamp
// are stepped over, and a probe that runs out of stamped lines has landed past
// the answer. The caller still filters by timestamp; this only decides where
// to start reading.
func tailFrom(f io.ReaderAt, size int64, t time.Time) int64 {
	lo, hi := int64(0), size
	for lo < hi {
		mid := lo + (hi-lo)/2
		r := bufio.NewReader(io.NewSectionReader(f, mid, size-mid))
		pos := mid
		if mid > 0 {
			skipped, err := r.ReadBytes('\n')
			pos += int64(len(skipped))
			if err != nil {
				hi = mid
				continue
			}
		}
		found := false
		for {
			line, err := r.ReadBytes('\n')
			pos += int64(len(line))
			if stamp, ok := stampOf(line); ok {
				if stamp.Before(t) {
					lo = pos
				} else {
					hi = mid
				}
				found = true
				break
			}
			if err != nil {
				break
			}
		}
		if !found {
			hi = mid
		}
	}
	return lo
}
