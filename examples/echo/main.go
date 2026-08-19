// Command echo is a two-process demonstration of iroh-go.
//
// In one terminal:
//
//	go run ./examples/echo listen
//
// It prints a ticket. In another terminal, paste it:
//
//	go run ./examples/echo dial <ticket> "hello from over there"
//
// Add -offline to both to stay on the loopback interface with relays and
// address lookup turned off, which is useful with no internet connection.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"net/netip"
	"os"
	"os/signal"
	"time"

	"github.com/discobox-ai/iroh-go"
)

const alpn = "iroh-go/echo/1"

func main() {
	offline := flag.Bool("offline", false, "bind to loopback with relays and address lookup disabled")
	verbose := flag.Bool("v", false, "log iroh's internal tracing at debug level")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage:\n  %s [flags] listen\n  %s [flags] dial <ticket> <message>\n\nflags:\n", os.Args[0], os.Args[0])
		flag.PrintDefaults()
	}
	flag.Parse()

	if *verbose {
		if err := iroh.SetLogLevel(iroh.LogDebug); err != nil {
			log.Printf("could not enable logging: %v", err)
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	var err error
	switch flag.Arg(0) {
	case "listen":
		err = listen(ctx, *offline)
	case "dial":
		if flag.NArg() < 3 {
			flag.Usage()
			os.Exit(2)
		}
		err = dial(ctx, *offline, iroh.Ticket(flag.Arg(1)), flag.Arg(2))
	default:
		flag.Usage()
		os.Exit(2)
	}
	if err != nil {
		log.Fatal(err)
	}
}

func options(offline bool, alpns ...string) iroh.Options {
	opts := iroh.Options{}
	for _, a := range alpns {
		opts.ALPNs = append(opts.ALPNs, []byte(a))
	}
	if offline {
		opts.Preset = iroh.PresetMinimal
		opts.RelayMode = iroh.RelayDisabled
		opts.BindAddrs = []netip.AddrPort{netip.MustParseAddrPort("127.0.0.1:0")}
	}
	return opts
}

func listen(ctx context.Context, offline bool) error {
	ep, err := iroh.Bind(ctx, options(offline, alpn))
	if err != nil {
		return fmt.Errorf("bind: %w", err)
	}
	defer shutdown(ep)

	if !offline {
		// Wait for a home relay so the ticket is dialable from anywhere.
		onlineCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		if err := ep.Online(onlineCtx); err != nil {
			log.Printf("continuing without a relay: %v", err)
		}
		cancel()
	}

	ticket, err := ep.Ticket()
	if err != nil {
		return fmt.Errorf("ticket: %w", err)
	}
	fmt.Printf("listening as %s\n\ndial me with:\n  echo dial %s \"your message\"\n\n", ep.ID().Short(), ticket)

	for {
		conn, err := ep.Accept(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("accept: %w", err)
		}
		go serve(ctx, conn)
	}
}

func serve(ctx context.Context, conn *iroh.Conn) {
	defer conn.Close()

	peer, err := conn.RemoteID()
	if err != nil {
		log.Printf("remote id: %v", err)
		return
	}
	log.Printf("connection from %s", peer.Short())

	send, recv, err := conn.AcceptBi(ctx)
	if err != nil {
		log.Printf("accept stream: %v", err)
		return
	}
	// SendStream and RecvStream are an io.Writer and io.Reader, so echoing
	// is just a copy.
	if _, err := io.Copy(send, recv); err != nil {
		log.Printf("echo: %v", err)
		return
	}
	if err := send.Close(); err != nil {
		log.Printf("finish: %v", err)
	}
}

func dial(ctx context.Context, offline bool, ticket iroh.Ticket, message string) error {
	ep, err := iroh.Bind(ctx, options(offline))
	if err != nil {
		return fmt.Errorf("bind: %w", err)
	}
	defer shutdown(ep)

	conn, err := ep.ConnectTicket(ctx, ticket, []byte(alpn))
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer conn.Close()

	send, recv, err := conn.OpenBi(ctx)
	if err != nil {
		return fmt.Errorf("open stream: %w", err)
	}
	if _, err := io.WriteString(send, message); err != nil {
		return fmt.Errorf("write: %w", err)
	}
	// Finishing the send half is what tells the peer the message is complete.
	if err := send.Close(); err != nil {
		return fmt.Errorf("finish: %w", err)
	}

	reply, err := io.ReadAll(recv)
	if err != nil {
		return fmt.Errorf("read: %w", err)
	}
	fmt.Printf("echoed back: %s\n", reply)
	return nil
}

func shutdown(ep *iroh.Endpoint) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := ep.Close(ctx); err != nil {
		log.Printf("close: %v", err)
	}
}
