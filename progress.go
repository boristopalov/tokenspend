package main

import (
	"fmt"
	"os"
	"time"

	"tokenspend/internal/spend"
)

// progress shows what a command is doing on stderr. On a terminal it
// redraws one status line; otherwise it prints a line per stage. A nil
// *progress (from -quiet) prints nothing.
type progress struct {
	tty    bool
	start  time.Time
	stage  string
	drawn  time.Time // last terminal redraw
	onLine bool      // a status line is on screen
}

func newProgress(quiet bool) *progress {
	if quiet {
		return nil
	}
	fi, err := os.Stderr.Stat()
	return &progress{tty: err == nil && fi.Mode()&os.ModeCharDevice != 0, start: time.Now()}
}

// update is a spend.Options.Progress callback.
func (p *progress) update(pr spend.Progress) {
	if p == nil {
		return
	}
	var text string
	switch pr.Stage {
	case spend.StageListing:
		text = "finding log files"
	case spend.StageReading:
		text = fmt.Sprintf("reading logs: %d/%d files, %s/%s", pr.Files, pr.TotalFiles, mb(pr.Bytes), mb(pr.TotalBytes))
		if pr.TotalBytes > 0 {
			text += fmt.Sprintf(" (%d%%)", pr.Bytes*100/pr.TotalBytes)
		}
		if !p.tty && pr.Stage != p.stage {
			// Piped output gets one line per stage, so say how much there is.
			text = fmt.Sprintf("reading %d log files (%s)", pr.TotalFiles, mb(pr.TotalBytes))
		}
	case spend.StageOTel:
		text = "merging OTel events"
	case spend.StageAttributing:
		text = "attributing context"
	default:
		text = pr.Stage
	}
	p.show(pr.Stage, text)
}

// step reports a stage of the command itself, after loading.
func (p *progress) step(stage string) {
	if p != nil {
		p.show(stage, stage)
	}
}

func (p *progress) show(stage, text string) {
	changed := stage != p.stage
	p.stage = stage
	line := fmt.Sprintf("[%4.1fs] %s", time.Since(p.start).Seconds(), text)
	if !p.tty {
		if changed {
			fmt.Fprintln(os.Stderr, line)
		}
		return
	}
	if !changed && time.Since(p.drawn) < 100*time.Millisecond {
		return
	}
	p.drawn = time.Now()
	fmt.Fprint(os.Stderr, "\r\033[K"+line)
	p.onLine = true
}

// clear removes the status line so normal output starts on a clean line.
func (p *progress) clear() {
	if p != nil && p.onLine {
		fmt.Fprint(os.Stderr, "\r\033[K")
		p.onLine = false
	}
}

// elapsed is how long the command has run, for the final message.
func (p *progress) elapsed() string {
	if p == nil {
		return ""
	}
	return fmt.Sprintf(" in %.1fs", time.Since(p.start).Seconds())
}

func mb(n int64) string { return fmt.Sprintf("%.0f MB", float64(n)/(1<<20)) }
