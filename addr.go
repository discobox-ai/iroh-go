package iroh

import (
	"net/netip"
	"strings"

	"github.com/discobox-ai/iroh-go/internal/ffi"
)

// EndpointAddr is everything needed to dial an endpoint: who it is, and
// optionally where to find it.
//
// The [ID] alone is often enough, because iroh can look the rest up through
// its relay and DNS address-lookup services. The other fields short-circuit
// that when you already know the answer, which is what makes purely local
// connections possible.
type EndpointAddr struct {
	// ID identifies the endpoint. Always required.
	ID EndpointID

	// RelayURL is the endpoint's home relay, used to reach it before (or
	// instead of) a direct connection.
	RelayURL string

	// DirectAddrs are UDP addresses the endpoint may be reachable at.
	DirectAddrs []netip.AddrPort

	// extra carries transport addresses this type does not model -- further
	// relay URLs and custom transports -- so that decoding and re-encoding
	// an address, or a ticket, never silently drops information.
	extra []string
}

// AddrOf returns the address of an endpoint known only by id.
func AddrOf(id EndpointID) EndpointAddr {
	return EndpointAddr{ID: id}
}

// WithDirectAddrs returns a copy of a with the given direct addresses added.
func (a EndpointAddr) WithDirectAddrs(addrs ...netip.AddrPort) EndpointAddr {
	a.DirectAddrs = append(append([]netip.AddrPort(nil), a.DirectAddrs...), addrs...)
	return a
}

// WithRelayURL returns a copy of a using the given home relay.
func (a EndpointAddr) WithRelayURL(url string) EndpointAddr {
	a.RelayURL = url
	return a
}

// IsEmpty reports whether the address carries no way to reach the endpoint
// beyond its id.
func (a EndpointAddr) IsEmpty() bool {
	return a.RelayURL == "" && len(a.DirectAddrs) == 0 && len(a.extra) == 0
}

// encode renders the line-oriented form the library exchanges. See
// rust/irohgo-ffi/src/addr.rs for the format.
func (a EndpointAddr) encode() string {
	var b strings.Builder
	b.WriteString(a.ID.String())
	if a.RelayURL != "" {
		b.WriteString("\nrelay ")
		b.WriteString(a.RelayURL)
	}
	for _, addr := range a.DirectAddrs {
		b.WriteString("\nip ")
		b.WriteString(addr.String())
	}
	for _, line := range a.extra {
		b.WriteString("\n")
		b.WriteString(line)
	}
	return b.String()
}

func decodeAddr(text string) (EndpointAddr, error) {
	lines := strings.Split(strings.TrimSpace(text), "\n")
	if len(lines) == 0 || lines[0] == "" {
		return EndpointAddr{}, errKind(KindInvalidInput, "endpoint address is empty")
	}
	id, err := ParseEndpointID(strings.TrimSpace(lines[0]))
	if err != nil {
		return EndpointAddr{}, err
	}

	addr := EndpointAddr{ID: id}
	for _, line := range lines[1:] {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		kind, rest, ok := strings.Cut(line, " ")
		if !ok {
			return EndpointAddr{}, errKind(KindInvalidInput, "malformed address line %q", line)
		}
		switch {
		case kind == "relay" && addr.RelayURL == "":
			addr.RelayURL = rest
		case kind == "ip":
			ap, err := netip.ParseAddrPort(rest)
			if err != nil {
				return EndpointAddr{}, errKind(KindInvalidInput, "bad direct address %q: %v", rest, err)
			}
			addr.DirectAddrs = append(addr.DirectAddrs, ap)
		default:
			// Additional relays and custom transports, kept verbatim.
			addr.extra = append(addr.extra, line)
		}
	}
	return addr, nil
}

// Validate checks the address against iroh's own parser, returning it
// normalised. Useful for reporting a bad address where it was built rather
// than at the point it is dialled.
func (a EndpointAddr) Validate() (EndpointAddr, error) {
	text, err := ffi.AddrNormalize(a.encode())
	if err != nil {
		return EndpointAddr{}, err
	}
	return decodeAddr(text)
}

// String renders the address in the same line-oriented form used across the
// FFI boundary. For something to paste into a chat window, use [Ticket].
func (a EndpointAddr) String() string { return a.encode() }

// Ticket encodes the address as an iroh ticket, the shareable string form
// other iroh implementations understand.
func (a EndpointAddr) Ticket() (Ticket, error) {
	s, err := ffi.TicketEncode(a.encode())
	if err != nil {
		return "", err
	}
	return Ticket(s), nil
}

// Ticket is an iroh endpoint ticket: a printable string carrying an
// [EndpointAddr].
type Ticket string

// ParseTicket validates a ticket string.
func ParseTicket(s string) (Ticket, error) {
	t := Ticket(strings.TrimSpace(s))
	if _, err := t.Addr(); err != nil {
		return "", err
	}
	return t, nil
}

// Addr decodes the address the ticket carries.
func (t Ticket) Addr() (EndpointAddr, error) {
	text, err := ffi.TicketDecode(string(t))
	if err != nil {
		return EndpointAddr{}, err
	}
	return decodeAddr(text)
}

func (t Ticket) String() string { return string(t) }
