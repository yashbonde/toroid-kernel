package main

import (
	"context"
	"fmt"
	"io"
	"text/tabwriter"
	"time"

	toroid "github.com/yashbonde/toroid-kernel"
)

// runSessions implements `trk sessions`. It lists every persisted session in the
// SQLite store, newest first, with wall duration and total cost. Output is
// tab-separated so it pipes cleanly into other tools.
func runSessions(ctx context.Context, out io.Writer, args []string) error {
	if len(args) != 0 {
		return fmt.Errorf("takes no arguments")
	}

	sessions, err := toroid.ListSessions()
	if err != nil {
		return err
	}
	if len(sessions) == 0 {
		fmt.Fprintln(out, "No sessions in the store.")
		return nil
	}

	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tSTARTED\tDURATION\tCOST\tTITLE")
	for _, s := range sessions {
		started := "—"
		if s.StartedAt > 0 {
			started = time.Unix(0, s.StartedAt).Format("Jan 02 15:04")
		}
		duration := s.DurationFmt()
		title := s.Title
		if title == "" {
			title = "(no title)"
		}
		// Truncate very long titles to keep the table readable.
		if len(title) > 60 {
			title = title[:57] + "..."
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t$%.4f\t%s\n",
			shortID(s.ID), started, duration, s.TotalUSD, title)
	}
	return w.Flush()
}

func shortID(id string) string {
	if len(id) <= 8 {
		return id
	}
	return id[:8]
}
