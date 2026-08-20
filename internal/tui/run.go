package tui

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"golang.org/x/term"
)

// RefreshInterval is how often the dashboard repaints. The numbers on screen
// are ages and statuses, so a second is plenty and keeps the process idle.
const RefreshInterval = time.Second

// Mode is which screen to draw.
type Mode int

const (
	// Summary is what `drover serve` shows: counts and health.
	Summary Mode = iota
	// Detail is what `drover dash` shows: the full tables.
	Detail
)

// Source supplies the current state and the two actions the screen offers.
type Source interface {
	// Snapshot is the state to draw. It is called on every repaint, so it
	// must be cheap and must not block on the network.
	Snapshot() Model

	// Reload re-reads the configured sources and applies them, which is what
	// someone wants after editing a yaml file.
	Reload() (string, error)

	// SyncAll asks every repository to refresh now.
	SyncAll() error
}

// Runner drives the dashboard.
type Runner struct {
	Source Source
	Mode   Mode
	In     *os.File
	Out    io.Writer

	// AutoReload says the engine re-applies edits on its own, so the screen
	// offers no reload key and says as much.
	AutoReload bool
}

// Supported reports whether a dashboard can be drawn at all. Without a
// terminal on both ends there is nothing to draw on and no keys to read, and
// serve falls back to plain log lines.
func Supported(in *os.File, out *os.File) bool {
	if in == nil || out == nil {
		return false
	}
	if os.Getenv("TERM") == "dumb" || os.Getenv("NO_TUI") != "" {
		return false
	}
	return term.IsTerminal(int(in.Fd())) && term.IsTerminal(int(out.Fd()))
}

// Run paints until the context is cancelled or the user quits. It returns nil
// on a clean quit, so the caller can shut the engine down normally.
func (r *Runner) Run(ctx context.Context) error {
	fd := int(r.In.Fd())

	// Raw mode, so a keypress arrives without waiting for Enter. The restore
	// is deferred first, before anything can fail, or a crash leaves the
	// user's terminal unusable.
	state, err := term.MakeRaw(fd)
	if err != nil {
		return fmt.Errorf("could not put the terminal in raw mode: %w", err)
	}
	defer term.Restore(fd, state)

	fmt.Fprint(r.Out, altScreen+hideCur)
	defer fmt.Fprint(r.Out, showCur+mainScr)

	keys := make(chan byte, 8)
	go readKeys(r.In, keys)

	ticker := time.NewTicker(RefreshInterval)
	defer ticker.Stop()

	var notice string
	var noticeKind string
	var noticeUntil time.Time
	reloading := false
	// Mode is toggled by a key, so it is local rather than read off the
	// Runner: serve opens on the summary, dash opens on the detail, and
	// either can switch without restarting anything.
	mode := r.Mode

	// A reload runs off the paint loop so a slow clone does not freeze the
	// screen; the result comes back here.
	type actionResult struct {
		msg string
		err error
	}
	results := make(chan actionResult, 4)

	paint := func() {
		m := r.Source.Snapshot()
		if !noticeUntil.IsZero() && time.Now().After(noticeUntil) {
			notice, noticeKind = "", ""
			noticeUntil = time.Time{}
		}
		if m.Notice == "" {
			m.Notice, m.NoticeKind = notice, noticeKind
		}
		m.Reloading = reloading
		m.AutoReload = r.AutoReload

		if mode == Summary {
			fmt.Fprint(r.Out, crlf(RenderSummary(m, width(r.In))))
			return
		}
		fmt.Fprint(r.Out, crlf(Render(m, width(r.In))))
	}
	paint()

	for {
		select {
		case <-ctx.Done():
			return nil

		case <-ticker.C:
			paint()

		case res := <-results:
			reloading = false
			if res.err != nil {
				notice, noticeKind = res.err.Error(), "err"
			} else {
				notice, noticeKind = res.msg, "ok"
			}
			noticeUntil = time.Now().Add(6 * time.Second)
			paint()

		case k, ok := <-keys:
			if !ok {
				return nil
			}
			switch k {
			case 'q', 'Q', 3: // 3 is ctrl-c, which raw mode delivers as a byte
				return nil

			case 'd', 'D':
				if mode == Summary {
					mode = Detail
				} else {
					mode = Summary
				}
				paint()

			case 'r', 'R':
				if r.AutoReload || reloading {
					// Nothing to press when edits apply themselves.
					break
				}
				reloading = true
				notice, noticeKind = "", ""
				paint()
				go func() {
					msg, err := r.Source.Reload()
					results <- actionResult{msg: msg, err: err}
				}()

			case 's', 'S':
				if err := r.Source.SyncAll(); err != nil {
					notice, noticeKind = err.Error(), "err"
				} else {
					notice, noticeKind = "refresh queued for every repository", "ok"
				}
				noticeUntil = time.Now().Add(6 * time.Second)
				paint()

			case 12: // ctrl-l, the conventional "redraw"
				paint()
			}
		}
	}
}

// readKeys forwards single bytes until stdin closes.
//
// Escape sequences (arrows, function keys) arrive as several bytes; the
// dashboard has no use for them, and forwarding the bytes individually means
// a stray sequence cannot be mistaken for a command -- 'q' is only ever the
// letter q, never the tail of an arrow key, because no arrow sequence ends
// in q.
func readKeys(in *os.File, out chan<- byte) {
	defer close(out)
	buf := make([]byte, 16)
	for {
		n, err := in.Read(buf)
		if n > 0 {
			for _, b := range buf[:n] {
				// Drop the CSI introducer and its parameter bytes rather than
				// letting them land as commands.
				if b == 0x1b {
					break
				}
				out <- b
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return
			}
			return
		}
	}
}

// crlf turns every newline into a carriage return plus newline.
//
// Raw mode clears ONLCR, so the terminal stops translating "\n" into "move
// down AND return to column 0" -- it only moves down. Text written with plain
// newlines then walks diagonally across the screen, each line starting where
// the previous one ended. The renderers stay readable and testable with plain
// "\n"; the translation happens once, here, on the way to the terminal.
func crlf(s string) string {
	// Normalise first so a string that already has \r\n does not become
	// \r\r\n, which some terminals render as a blank line.
	s = strings.ReplaceAll(s, "\r\n", "\n")
	return strings.ReplaceAll(s, "\n", "\r\n")
}

func width(f *os.File) int {
	w, _, err := term.GetSize(int(f.Fd()))
	if err != nil || w <= 0 {
		return 100
	}
	if w > 160 {
		return 160
	}
	return w
}
