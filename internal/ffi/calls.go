package ffi

import (
	"context"
	"runtime"
	"unsafe"
)

// Every wrapper below keeps its input buffers alive across the call with
// runtime.KeepAlive. purego hands the library a bare pointer, which on its
// own is not enough to stop the collector from reclaiming the backing array.

// -- library-wide ----------------------------------------------------------

// SetLogLevel installs a tracing subscriber. Only the first call has effect.
func SetLogLevel(level int32) error {
	if err := Load(); err != nil {
		return err
	}
	if c.setLogLevel(level) != 0 {
		return NewError(KindInvalidInput, "iroh: could not set log level")
	}
	return nil
}

// IrohVersion reports the version of the iroh crate the library was built
// against.
func IrohVersion() (string, error) {
	if err := Load(); err != nil {
		return "", err
	}
	var (
		ptr unsafe.Pointer
		n   uintptr
	)
	c.irohVersion(unsafe.Pointer(&ptr), unsafe.Pointer(&n))
	return takeString(ptr, n), nil
}

// HandleCount reports the number of live objects inside the library.
func HandleCount() (uint64, error) {
	if err := Load(); err != nil {
		return 0, err
	}
	return c.handleCount(), nil
}

// Shutdown stops the completion pump, making any wait return "shutting down".
//
// A process that is exiting does not need this; it exists so tests can prove
// the pump's exit path works. Operations still in flight afterwards never
// complete, so call it only when nothing is outstanding.
func Shutdown() {
	if loadErr == nil && c.completionWake != nil {
		c.completionWake()
	}
}

// -- keys ------------------------------------------------------------------

func SecretKeyGenerate(out []byte) error {
	if err := Load(); err != nil {
		return err
	}
	c.secretKeyGenerate(bytePtr(out))
	runtime.KeepAlive(out)
	return nil
}

func SecretKeyPublic(key, outID []byte) error {
	if err := Load(); err != nil {
		return err
	}
	var errh uint64
	rc := c.secretKeyPublic(bytePtr(key), bytePtr(outID), unsafe.Pointer(&errh))
	runtime.KeepAlive(key)
	runtime.KeepAlive(outID)
	if rc != 0 {
		return takeError(errh)
	}
	return nil
}

func SecretKeySign(key, msg, outSig []byte) error {
	if err := Load(); err != nil {
		return err
	}
	var errh uint64
	rc := c.secretKeySign(bytePtr(key), bytePtr(msg), uintptr(len(msg)), bytePtr(outSig), unsafe.Pointer(&errh))
	runtime.KeepAlive(key)
	runtime.KeepAlive(msg)
	runtime.KeepAlive(outSig)
	if rc != 0 {
		return takeError(errh)
	}
	return nil
}

func EndpointIDVerify(id, msg, sig []byte) error {
	if err := Load(); err != nil {
		return err
	}
	var errh uint64
	rc := c.endpointIDVerify(bytePtr(id), bytePtr(msg), uintptr(len(msg)), bytePtr(sig), unsafe.Pointer(&errh))
	runtime.KeepAlive(id)
	runtime.KeepAlive(msg)
	runtime.KeepAlive(sig)
	if rc != 0 {
		return takeError(errh)
	}
	return nil
}

func EndpointIDFormat(id []byte) (string, error) {
	if err := Load(); err != nil {
		return "", err
	}
	var (
		ptr  unsafe.Pointer
		n    uintptr
		errh uint64
	)
	rc := c.endpointIDFormat(bytePtr(id), unsafe.Pointer(&ptr), unsafe.Pointer(&n), unsafe.Pointer(&errh))
	runtime.KeepAlive(id)
	if rc != 0 {
		return "", takeError(errh)
	}
	return takeString(ptr, n), nil
}

func EndpointIDParse(s string, outID []byte) error {
	if err := Load(); err != nil {
		return err
	}
	in := []byte(s)
	var errh uint64
	rc := c.endpointIDParse(bytePtr(in), uintptr(len(in)), bytePtr(outID), unsafe.Pointer(&errh))
	runtime.KeepAlive(in)
	runtime.KeepAlive(outID)
	if rc != 0 {
		return takeError(errh)
	}
	return nil
}

// -- addresses and tickets -------------------------------------------------

// text is the shape shared by the address helpers: they all take a buffer and
// produce an owned string.
func textCall(in []byte, call func(inPtr unsafe.Pointer, inLen uintptr, outStr, outLen, outErr unsafe.Pointer) int32) (string, error) {
	if err := Load(); err != nil {
		return "", err
	}
	var (
		ptr  unsafe.Pointer
		n    uintptr
		errh uint64
	)
	rc := call(bytePtr(in), uintptr(len(in)), unsafe.Pointer(&ptr), unsafe.Pointer(&n), unsafe.Pointer(&errh))
	runtime.KeepAlive(in)
	if rc != 0 {
		return "", takeError(errh)
	}
	return takeString(ptr, n), nil
}

// AddrNormalize validates an address in text form and returns it normalised.
func AddrNormalize(text string) (string, error) {
	return textCall([]byte(text), func(p unsafe.Pointer, n uintptr, a, b, e unsafe.Pointer) int32 {
		return c.addrNormalize(p, n, a, b, e)
	})
}

// TicketEncode turns an address in text form into a ticket string.
func TicketEncode(text string) (string, error) {
	return textCall([]byte(text), func(p unsafe.Pointer, n uintptr, a, b, e unsafe.Pointer) int32 {
		return c.ticketEncode(p, n, a, b, e)
	})
}

// TicketDecode turns a ticket string back into an address in text form.
func TicketDecode(ticket string) (string, error) {
	return textCall([]byte(ticket), func(p unsafe.Pointer, n uintptr, a, b, e unsafe.Pointer) int32 {
		return c.ticketDecode(p, n, a, b, e)
	})
}

// -- options ---------------------------------------------------------------

func OptionsNew(preset int32) (uint64, error) {
	if err := Load(); err != nil {
		return 0, err
	}
	return c.optionsNew(preset), nil
}

func OptionsFree(h uint64) { c.optionsFree(h) }

func OptionsSetSecretKey(h uint64, key []byte) error {
	var errh uint64
	rc := c.optionsSetSecretKey(h, bytePtr(key), unsafe.Pointer(&errh))
	runtime.KeepAlive(key)
	if rc != 0 {
		return takeError(errh)
	}
	return nil
}

func OptionsAddALPN(h uint64, alpn []byte) error {
	var errh uint64
	rc := c.optionsAddALPN(h, bytePtr(alpn), uintptr(len(alpn)), unsafe.Pointer(&errh))
	runtime.KeepAlive(alpn)
	if rc != 0 {
		return takeError(errh)
	}
	return nil
}

func OptionsSetRelayMode(h uint64, mode int32) error {
	var errh uint64
	if c.optionsSetRelayMode(h, mode, unsafe.Pointer(&errh)) != 0 {
		return takeError(errh)
	}
	return nil
}

func OptionsAddRelayURL(h uint64, url string) error {
	in := []byte(url)
	var errh uint64
	rc := c.optionsAddRelayURL(h, bytePtr(in), uintptr(len(in)), unsafe.Pointer(&errh))
	runtime.KeepAlive(in)
	if rc != 0 {
		return takeError(errh)
	}
	return nil
}

func OptionsAddBindAddr(h uint64, addr string) error {
	in := []byte(addr)
	var errh uint64
	rc := c.optionsAddBindAddr(h, bytePtr(in), uintptr(len(in)), unsafe.Pointer(&errh))
	runtime.KeepAlive(in)
	if rc != 0 {
		return takeError(errh)
	}
	return nil
}

// -- endpoint --------------------------------------------------------------

// EndpointBind consumes options whether or not it succeeds.
func EndpointBind(ctx context.Context, options uint64) (uint64, error) {
	return AwaitHandle(ctx, c.endpointBind(options))
}

func EndpointID(h uint64, outID []byte) error {
	var errh uint64
	rc := c.endpointID(h, bytePtr(outID), unsafe.Pointer(&errh))
	runtime.KeepAlive(outID)
	if rc != 0 {
		return takeError(errh)
	}
	return nil
}

func EndpointAddr(h uint64) (string, error) {
	var (
		ptr  unsafe.Pointer
		n    uintptr
		errh uint64
	)
	if c.endpointAddr(h, unsafe.Pointer(&ptr), unsafe.Pointer(&n), unsafe.Pointer(&errh)) != 0 {
		return "", takeError(errh)
	}
	return takeString(ptr, n), nil
}

func EndpointBoundSockets(h uint64) (string, error) {
	var (
		ptr  unsafe.Pointer
		n    uintptr
		errh uint64
	)
	if c.endpointSockets(h, unsafe.Pointer(&ptr), unsafe.Pointer(&n), unsafe.Pointer(&errh)) != 0 {
		return "", takeError(errh)
	}
	return takeString(ptr, n), nil
}

func EndpointOnline(ctx context.Context, h uint64) error {
	return AwaitUnit(ctx, c.endpointOnline(h))
}

func EndpointHomeRelay(h uint64) (string, error) {
	var (
		ptr  unsafe.Pointer
		n    uintptr
		errh uint64
	)
	if c.endpointHomeRelay(h, unsafe.Pointer(&ptr), unsafe.Pointer(&n), unsafe.Pointer(&errh)) != 0 {
		return "", takeError(errh)
	}
	return takeString(ptr, n), nil
}

func EndpointConnect(ctx context.Context, h uint64, addr string, alpn []byte) (uint64, error) {
	in := []byte(addr)
	op := c.endpointConnect(h, bytePtr(in), uintptr(len(in)), bytePtr(alpn), uintptr(len(alpn)))
	runtime.KeepAlive(in)
	runtime.KeepAlive(alpn)
	return AwaitHandle(ctx, op)
}

func EndpointAccept(ctx context.Context, h uint64) (uint64, error) {
	return AwaitHandle(ctx, c.endpointAccept(h))
}

func EndpointClose(ctx context.Context, h uint64) error {
	return AwaitUnit(ctx, c.endpointClose(h))
}

func EndpointFree(h uint64) { c.endpointFree(h) }

// -- connection ------------------------------------------------------------

func ConnOpenBi(ctx context.Context, h uint64) (uint64, uint64, error) {
	return AwaitHandle2(ctx, c.connOpenBi(h))
}

func ConnAcceptBi(ctx context.Context, h uint64) (uint64, uint64, error) {
	return AwaitHandle2(ctx, c.connAcceptBi(h))
}

func ConnOpenUni(ctx context.Context, h uint64) (uint64, error) {
	return AwaitHandle(ctx, c.connOpenUni(h))
}

func ConnAcceptUni(ctx context.Context, h uint64) (uint64, error) {
	return AwaitHandle(ctx, c.connAcceptUni(h))
}

func ConnSendDatagram(h uint64, data []byte) error {
	var errh uint64
	rc := c.connSendDatagram(h, bytePtr(data), uintptr(len(data)), unsafe.Pointer(&errh))
	runtime.KeepAlive(data)
	if rc != 0 {
		return takeError(errh)
	}
	return nil
}

func ConnReadDatagram(ctx context.Context, h uint64) ([]byte, error) {
	data, _, err := AwaitBytes(ctx, c.connReadDatagram(h))
	return data, err
}

func ConnMaxDatagramSize(h uint64) (uint64, error) {
	var size, errh uint64
	if c.connMaxDatagramSize(h, unsafe.Pointer(&size), unsafe.Pointer(&errh)) != 0 {
		return 0, takeError(errh)
	}
	return size, nil
}

func ConnRemoteID(h uint64, outID []byte) error {
	var errh uint64
	rc := c.connRemoteID(h, bytePtr(outID), unsafe.Pointer(&errh))
	runtime.KeepAlive(outID)
	if rc != 0 {
		return takeError(errh)
	}
	return nil
}

func ConnALPN(h uint64) ([]byte, error) {
	var (
		ptr  unsafe.Pointer
		n    uintptr
		errh uint64
	)
	if c.connALPN(h, unsafe.Pointer(&ptr), unsafe.Pointer(&n), unsafe.Pointer(&errh)) != 0 {
		return nil, takeError(errh)
	}
	return takeBytes(ptr, n), nil
}

func ConnStats(h uint64) ([StatsLen]uint64, error) {
	var (
		stats [StatsLen]uint64
		errh  uint64
	)
	rc := c.connStats(h, unsafe.Pointer(&stats[0]), StatsLen, unsafe.Pointer(&errh))
	runtime.KeepAlive(&stats)
	if rc != 0 {
		return stats, takeError(errh)
	}
	return stats, nil
}

func ConnClose(h uint64, code uint64, reason []byte) error {
	var errh uint64
	rc := c.connClose(h, code, bytePtr(reason), uintptr(len(reason)), unsafe.Pointer(&errh))
	runtime.KeepAlive(reason)
	if rc != 0 {
		return takeError(errh)
	}
	return nil
}

func ConnClosed(ctx context.Context, h uint64) (string, error) {
	reason, _, err := AwaitBytes(ctx, c.connClosed(h))
	return string(reason), err
}

func ConnFree(h uint64) { c.connFree(h) }

// -- streams ---------------------------------------------------------------

func SendWrite(ctx context.Context, h uint64, data []byte) (uint64, error) {
	op := c.sendWrite(h, bytePtr(data), uintptr(len(data)))
	runtime.KeepAlive(data)
	return AwaitU64(ctx, op)
}

func SendFinish(ctx context.Context, h uint64) error {
	return AwaitUnit(ctx, c.sendFinish(h))
}

func SendReset(ctx context.Context, h uint64, code uint64) error {
	return AwaitUnit(ctx, c.sendReset(h, code))
}

func SendSetPriority(ctx context.Context, h uint64, priority int32) error {
	return AwaitUnit(ctx, c.sendSetPriority(h, priority))
}

func SendStopped(ctx context.Context, h uint64) (uint64, error) {
	return AwaitU64(ctx, c.sendStopped(h))
}

func SendFree(h uint64) { c.sendFree(h) }

// RecvRead reads into dst. The bool reports a clean end of stream.
//
// The library never returns more than the requested length, so a short dst
// cannot silently drop data.
func RecvRead(ctx context.Context, h uint64, dst []byte) (int, bool, error) {
	op := c.recvRead(h, uintptr(len(dst)))
	return AwaitBytesInto(ctx, op, dst)
}

func RecvStop(ctx context.Context, h uint64, code uint64) error {
	return AwaitUnit(ctx, c.recvStop(h, code))
}

func RecvFree(h uint64) { c.recvFree(h) }
