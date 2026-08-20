package iroh

import (
	"context"
	"net/netip"
	"runtime"
	"strings"

	"github.com/discobox-ai/iroh-go/internal/ffi"
)

// Preset selects a bundle of endpoint defaults.
//
// The zero value is [PresetN0], the configuration almost every application
// wants: n0's relays plus DNS and pkarr address lookup.
type Preset int

const (
	// PresetN0 uses number 0's public relay and address-lookup services.
	PresetN0 Preset = iota
	// PresetN0NoRelay is PresetN0 with relays turned off, so connections
	// must be direct.
	PresetN0NoRelay
	// PresetMinimal configures only a TLS provider: no relays, no address
	// lookup. Suitable when every peer address is known up front.
	PresetMinimal
	// PresetEmpty applies nothing at all. Advanced use.
	PresetEmpty
)

func (p Preset) ffi() (int32, error) {
	switch p {
	case PresetN0:
		return ffi.PresetN0, nil
	case PresetN0NoRelay:
		return ffi.PresetN0DisableRelay, nil
	case PresetMinimal:
		return ffi.PresetMinimal, nil
	case PresetEmpty:
		return ffi.PresetEmpty, nil
	default:
		return 0, errKind(KindInvalidInput, "unknown preset %d", p)
	}
}

// RelayMode selects which relay servers an endpoint uses. The zero value
// keeps whatever the [Preset] chose.
type RelayMode int

const (
	// RelayFromPreset leaves the preset's relay configuration alone.
	RelayFromPreset RelayMode = iota
	// RelayDefault uses number 0's production relays.
	RelayDefault
	// RelayDisabled turns relays off entirely.
	RelayDisabled
	// RelayStaging uses number 0's staging relays.
	RelayStaging
	// RelayCustom uses the relays listed in [Options.RelayURLs].
	RelayCustom
)

// Options configures an endpoint. The zero value is usable and produces an
// endpoint with n0's defaults and a fresh random identity.
type Options struct {
	// Preset selects the bundle of defaults to start from.
	Preset Preset

	// SecretKey is the endpoint's identity. A random key is generated when
	// this is nil, which means a new [EndpointID] on every run.
	SecretKey *SecretKey

	// ALPNs are the protocols this endpoint will accept connections for.
	// An endpoint that only dials does not need any.
	ALPNs [][]byte

	// RelayMode overrides the preset's relay configuration.
	RelayMode RelayMode

	// RelayURLs lists the relays to use when RelayMode is RelayCustom.
	RelayURLs []string

	// BindAddrs are the local UDP addresses to bind. When set, these replace
	// the preset's sockets entirely. Use a zero port to let the OS choose.
	BindAddrs []netip.AddrPort
}

// Endpoint is a local iroh endpoint: one identity, one set of sockets, and
// the connections made through them.
//
// An Endpoint is safe for concurrent use, with one exception noted on
// [Endpoint.Accept].
type Endpoint struct {
	h  handle
	id EndpointID
}

// Bind creates an endpoint and binds its sockets.
func Bind(ctx context.Context, opts Options) (*Endpoint, error) {
	preset, err := opts.Preset.ffi()
	if err != nil {
		return nil, err
	}
	options, err := ffi.OptionsNew(preset)
	if err != nil {
		return nil, err
	}
	// From here on the library owns the options object; Bind consumes it
	// even when it fails, so only the early-return paths free it.
	if err := applyOptions(options, opts); err != nil {
		ffi.OptionsFree(options)
		return nil, err
	}

	h, err := ffi.EndpointBind(ctx, options)
	if err != nil {
		return nil, err
	}

	ep := &Endpoint{}
	ep.h.set(h)
	if err := ffi.EndpointID(h, ep.id[:]); err != nil {
		ffi.EndpointFree(h)
		return nil, err
	}
	runtime.AddCleanup(ep, ffi.EndpointFree, h)
	return ep, nil
}

func applyOptions(options uint64, opts Options) error {
	if opts.SecretKey != nil {
		if err := ffi.OptionsSetSecretKey(options, opts.SecretKey[:]); err != nil {
			return err
		}
	}
	for _, alpn := range opts.ALPNs {
		if err := ffi.OptionsAddALPN(options, alpn); err != nil {
			return err
		}
	}
	if opts.RelayMode != RelayFromPreset {
		var mode int32
		switch opts.RelayMode {
		case RelayDefault:
			mode = ffi.RelayDefault
		case RelayDisabled:
			mode = ffi.RelayDisabled
		case RelayStaging:
			mode = ffi.RelayStaging
		case RelayCustom:
			mode = ffi.RelayCustom
		default:
			return errKind(KindInvalidInput, "unknown relay mode %d", opts.RelayMode)
		}
		for _, url := range opts.RelayURLs {
			if err := ffi.OptionsAddRelayURL(options, url); err != nil {
				return err
			}
		}
		if err := ffi.OptionsSetRelayMode(options, mode); err != nil {
			return err
		}
	}
	for _, addr := range opts.BindAddrs {
		if err := ffi.OptionsAddBindAddr(options, addr.String()); err != nil {
			return err
		}
	}
	return nil
}

// ID returns this endpoint's identity.
func (e *Endpoint) ID() EndpointID { return e.id }

// Addr returns the endpoint's current address.
//
// Local socket addresses are available as soon as Bind returns. The relay
// url appears once the endpoint is online; call [Endpoint.Online] first if
// the address is going to be handed to a peer on another network.
func (e *Endpoint) Addr() (EndpointAddr, error) {
	h, err := e.h.get()
	if err != nil {
		return EndpointAddr{}, err
	}
	text, err := ffi.EndpointAddr(h)
	if err != nil {
		return EndpointAddr{}, err
	}
	return decodeAddr(text)
}

// BoundSockets are the local addresses the endpoint's sockets are bound to.
//
// This is what the operating system gave us, which is a different question
// from [Endpoint.Addr]: that one reports where peers should reach this
// endpoint, and it can still be filling in or be empty on a machine with no
// usable interface. A caller that has to hand a peer something dialable right
// now wants both, and a wildcard here (0.0.0.0 or [::]) is a bind address
// rather than a dial target -- rewrite it to loopback before publishing it.
func (e *Endpoint) BoundSockets() ([]netip.AddrPort, error) {
	h, err := e.h.get()
	if err != nil {
		return nil, err
	}
	text, err := ffi.EndpointBoundSockets(h)
	if err != nil {
		return nil, err
	}
	if text == "" {
		return nil, nil
	}
	lines := strings.Split(text, "\n")
	out := make([]netip.AddrPort, 0, len(lines))
	for _, line := range lines {
		addr, err := netip.ParseAddrPort(line)
		if err != nil {
			return nil, errKind(KindInternal, "unparsable bound socket %q: %v", line, err)
		}
		out = append(out, addr)
	}
	return out, nil
}

// Online waits until the endpoint has connected to a relay.
//
// It never returns on its own when relays are disabled or there is no
// internet connection, so always pass a context with a deadline.
func (e *Endpoint) Online(ctx context.Context) error {
	h, err := e.h.get()
	if err != nil {
		return err
	}
	return ffi.EndpointOnline(ctx, h)
}

// HomeRelay returns the endpoint's home relay url, or "" if it has none yet.
func (e *Endpoint) HomeRelay() (string, error) {
	h, err := e.h.get()
	if err != nil {
		return "", err
	}
	return ffi.EndpointHomeRelay(h)
}

// Ticket returns a shareable ticket for the endpoint's current address.
func (e *Endpoint) Ticket() (Ticket, error) {
	addr, err := e.Addr()
	if err != nil {
		return "", err
	}
	return addr.Ticket()
}

// Connect dials addr and negotiates alpn.
func (e *Endpoint) Connect(ctx context.Context, addr EndpointAddr, alpn []byte) (*Conn, error) {
	h, err := e.h.get()
	if err != nil {
		return nil, err
	}
	if len(alpn) == 0 {
		return nil, errKind(KindALPN, "connect requires an alpn")
	}
	c, err := ffi.EndpointConnect(ctx, h, addr.encode(), alpn)
	if err != nil {
		return nil, err
	}
	return newConn(c), nil
}

// ConnectTicket dials the endpoint a ticket points at.
func (e *Endpoint) ConnectTicket(ctx context.Context, ticket Ticket, alpn []byte) (*Conn, error) {
	addr, err := ticket.Addr()
	if err != nil {
		return nil, err
	}
	return e.Connect(ctx, addr, alpn)
}

// Accept waits for the next inbound connection and completes its handshake.
//
// Unlike the rest of the type, Accept should be driven from a single
// goroutine; the usual shape is one accept loop handing each connection to
// its own goroutine. It returns an error wrapping [ErrClosed] once the
// endpoint is closed.
func (e *Endpoint) Accept(ctx context.Context) (*Conn, error) {
	h, err := e.h.get()
	if err != nil {
		return nil, err
	}
	c, err := ffi.EndpointAccept(ctx, h)
	if err != nil {
		return nil, err
	}
	return newConn(c), nil
}

// Close shuts the endpoint down, closing its connections. It is idempotent.
func (e *Endpoint) Close(ctx context.Context) error {
	h := e.h.take()
	if h == 0 {
		return nil
	}
	defer ffi.EndpointFree(h)
	return ffi.EndpointClose(ctx, h)
}
