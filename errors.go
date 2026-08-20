package iroh

import (
	"errors"
	"fmt"

	"github.com/discobox-ai/iroh-go/internal/ffi"
)

// Kind classifies an [Error]. Each kind has a matching sentinel that
// [errors.Is] recognises.
type Kind int

// Error kinds, mirroring the classification iroh's own bindings use.
const (
	KindInternal Kind = iota
	KindInvalidInput
	KindKeyParsing
	KindTicketParsing
	KindBind
	KindConnect
	KindConnection
	KindStream
	KindDatagram
	KindALPN
	KindRelay
	KindClosed
	KindTimeout
	KindCancelled
)

// Sentinel errors. Compare with [errors.Is]:
//
//	if errors.Is(err, iroh.ErrClosed) { ... }
var (
	ErrInternal      = errors.New("iroh: internal error")
	ErrInvalidInput  = errors.New("iroh: invalid input")
	ErrKeyParsing    = errors.New("iroh: key parsing failed")
	ErrTicketParsing = errors.New("iroh: ticket parsing failed")
	ErrBind          = errors.New("iroh: bind failed")
	ErrConnect       = errors.New("iroh: connect failed")
	ErrConnection    = errors.New("iroh: connection lost")
	ErrStream        = errors.New("iroh: stream error")
	ErrDatagram      = errors.New("iroh: datagram error")
	ErrALPN          = errors.New("iroh: alpn error")
	ErrRelay         = errors.New("iroh: relay error")
	ErrClosed        = errors.New("iroh: closed")
	ErrTimeout       = errors.New("iroh: timed out")
	ErrCancelled     = errors.New("iroh: cancelled")
)

var sentinels = map[Kind]error{
	KindInternal:      ErrInternal,
	KindInvalidInput:  ErrInvalidInput,
	KindKeyParsing:    ErrKeyParsing,
	KindTicketParsing: ErrTicketParsing,
	KindBind:          ErrBind,
	KindConnect:       ErrConnect,
	KindConnection:    ErrConnection,
	KindStream:        ErrStream,
	KindDatagram:      ErrDatagram,
	KindALPN:          ErrALPN,
	KindRelay:         ErrRelay,
	KindClosed:        ErrClosed,
	KindTimeout:       ErrTimeout,
	KindCancelled:     ErrCancelled,
}

// Error is the error type returned by every operation in this package that
// fails inside iroh itself.
type Error struct {
	Kind Kind
	Msg  string
}

func (e *Error) Error() string {
	// Both halves. The kind is what a caller branches on and the message is
	// what a person reads; printing only the message dropped the
	// classification, and an operation that failed without one printed a bare
	// "iroh: connection lost" with no hint of what had been attempted.
	kind := sentinelFor(e.Kind).Error()
	if e.Msg == "" {
		return kind
	}
	return kind + ": " + e.Msg
}

// Unwrap yields the sentinel for this error's kind, which is what makes
// errors.Is(err, ErrClosed) and friends work.
func (e *Error) Unwrap() error { return sentinelFor(e.Kind) }

func sentinelFor(k Kind) error {
	if s, ok := sentinels[k]; ok {
		return s
	}
	return ErrInternal
}

func init() {
	// The internal package raises errors but must not define the public
	// error type, so it calls back here to build them.
	ffi.NewError = func(kind int32, msg string) error {
		return &Error{Kind: Kind(kind), Msg: msg}
	}
}

// errKind builds an error of a given kind from this package.
func errKind(kind Kind, format string, args ...any) error {
	return &Error{Kind: kind, Msg: fmt.Sprintf(format, args...)}
}
