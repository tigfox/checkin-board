// Command uvk5probe talks to a UV-K5/UV-K1 radio directly, for bench
// diagnosis: it connects (handshake, capabilities, settings, monitor
// on), prints every event the radio reports, and optionally sends one
// message. With -raw it also prints every byte read from the port.
// Stop checkin-board first (or switch its radio type away): only one
// program can have the port.
//
//	uvk5probe [-port /dev/ttyACM0] [-listen 60s] [-raw] [-dtr-low] [-to CALL -text TEXT]
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"checkin-board/internal/uvk5"
)

func main() {
	port := flag.String("port", "", "serial port (default: the first radio found)")
	listen := flag.Duration("listen", time.Minute, "how long to print events")
	raw := flag.Bool("raw", false, "print every byte read")
	dtrLow := flag.Bool("dtr-low", false, "keep DTR low (firmware since 2026-10-13 sends monitor lines without it)")
	to := flag.String("to", "", "send a message to this callsign-SSID")
	text := flag.String("text", "uvk5probe test", "the message text")
	flag.Parse()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, *port, *listen, *raw, *dtrLow, *to, *text); err != nil {
		fmt.Fprintln(os.Stderr, "uvk5probe:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, port string, listen time.Duration, raw, dtrLow bool, to, text string) error {
	if port == "" {
		c := uvk5.Candidates()
		if len(c) == 0 {
			return fmt.Errorf("no radio port found")
		}
		port = c[0]
	}
	rw, err := uvk5.OpenPort(port)
	if err != nil {
		return err
	}
	if raw {
		rw = &tap{ReadWriteCloser: rw, start: time.Now()}
	}
	if dtrLow {
		rw = struct{ io.ReadWriteCloser }{rw} // hides SetDTR: the session can't raise it
	}
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	sess, err := uvk5.Connect(cctx, rw)
	cancel()
	if err != nil {
		_ = rw.Close()
		return err
	}
	defer sess.Close()
	fmt.Printf("connected on %s: firmware %q, caps %+v\nsettings %+v\n", port, sess.Version, sess.Caps, sess.Settings())
	if st, err := sess.Status(ctx); err == nil {
		fmt.Printf("status %+v\n", st)
	} else {
		fmt.Println("status:", err)
	}
	if to != "" {
		res, err := sess.Send(ctx, to, text)
		fmt.Printf("send to %s %q: %+v err %v\n", to, text, res, err)
	}
	deadline := time.After(listen)
	for {
		select {
		case ev := <-sess.Events():
			fmt.Printf("%s event %T %+v\n", time.Now().Format("15:04:05.000"), ev, ev)
		case <-sess.Done():
			return sess.Err()
		case <-deadline:
			if st, err := sess.Status(ctx); err == nil {
				fmt.Printf("status %+v\n", st)
			}
			return nil
		case <-ctx.Done():
			return nil
		}
	}
}

// tap prints what's read from the port.
type tap struct {
	io.ReadWriteCloser
	start time.Time
}

func (t *tap) Read(p []byte) (int, error) {
	n, err := t.ReadWriteCloser.Read(p)
	if n > 0 {
		var printable strings.Builder
		for _, c := range p[:n] {
			if c >= 0x20 && c < 0x7f {
				printable.WriteByte(c)
			} else {
				printable.WriteByte('.')
			}
		}
		fmt.Printf("%8.3f raw % x  |%s|\n", time.Since(t.start).Seconds(), p[:n], printable.String())
	}
	return n, err
}
