// Package ffi binds the iroh cdylib through purego and drives its completion
// queue.
//
// Everything here follows the ABI documented in rust/irohgo-ffi/src/ffi.rs:
// only scalars and pointers cross the boundary, fallible synchronous calls
// take a trailing error out-parameter, and asynchronous calls return an
// operation id that resolves through the completion queue.
package ffi

import (
	"fmt"
	"sync"
	"unsafe"

	"github.com/ebitengine/purego"

	"github.com/discobox-ai/iroh-go/internal/lib"
)

// ABIVersion must match iroh_abi_version() in the loaded library. Bumped
// whenever the ABI changes shape, so a mismatched pair of bindings and
// library is a clear error rather than a crash.
const ABIVersion = 3

// Completion statuses, matching error.rs.
const (
	StatusOK        int32 = 0
	StatusErr       int32 = 1
	StatusCancelled int32 = 2
)

// Error kinds, matching ErrKind in error.rs.
const (
	KindInternal int32 = iota
	KindInvalidInput
	KindKeyParsing
	KindTicketParsing
	KindBind
	KindConnect
	KindConnection
	KindStream
	KindDatagram
	KindAlpn
	KindRelay
	KindClosed
	KindTimeout
	KindCancelled
)

// Preset and relay-mode selectors, matching endpoint.rs.
const (
	PresetEmpty int32 = iota
	PresetMinimal
	PresetN0
	PresetN0DisableRelay
)

const (
	RelayDefault int32 = iota
	RelayDisabled
	RelayStaging
	RelayCustom
)

// StatsLen is the number of counters iroh_conn_stats writes.
const StatsLen = 6

// NoStopCode marks a send stream that finished without the peer stopping it.
const NoStopCode = ^uint64(0)

type api struct {
	abiVersion  func() uint32
	irohVersion func(outStr, outLen unsafe.Pointer)
	initRuntime func() int32
	setLogLevel func(level int32) int32
	freeBytes   func(ptr unsafe.Pointer, length uintptr)
	handleCount func() uint64

	completionWait func(timeoutMS int64, outOp, outStatus unsafe.Pointer) int32
	completionWake func()

	opCancel        func(op uint64)
	opFree          func(op uint64)
	opResultHandle  func(op uint64, out, outErr unsafe.Pointer) int32
	opResultHandle2 func(op uint64, outA, outB, outErr unsafe.Pointer) int32
	opResultU64     func(op uint64, out, outErr unsafe.Pointer) int32
	opResultBytes   func(op uint64, outPtr, outLen, outEOF, outErr unsafe.Pointer) int32
	opResultErr     func(op uint64, outErr unsafe.Pointer) int32
	errorTake       func(err uint64, outKind, outMsg, outLen unsafe.Pointer)

	secretKeyGenerate func(outKey unsafe.Pointer)
	secretKeyPublic   func(key, outID, outErr unsafe.Pointer) int32
	secretKeySign     func(key, msg unsafe.Pointer, msgLen uintptr, outSig, outErr unsafe.Pointer) int32
	endpointIDVerify  func(id, msg unsafe.Pointer, msgLen uintptr, sig, outErr unsafe.Pointer) int32
	endpointIDFormat  func(id, outStr, outLen, outErr unsafe.Pointer) int32
	endpointIDParse   func(s unsafe.Pointer, sLen uintptr, outID, outErr unsafe.Pointer) int32

	addrNormalize func(text unsafe.Pointer, textLen uintptr, outStr, outLen, outErr unsafe.Pointer) int32
	ticketEncode  func(text unsafe.Pointer, textLen uintptr, outStr, outLen, outErr unsafe.Pointer) int32
	ticketDecode  func(s unsafe.Pointer, sLen uintptr, outStr, outLen, outErr unsafe.Pointer) int32

	optionsNew          func(preset int32) uint64
	optionsFree         func(h uint64)
	optionsSetSecretKey func(h uint64, key, outErr unsafe.Pointer) int32
	optionsAddALPN      func(h uint64, alpn unsafe.Pointer, alpnLen uintptr, outErr unsafe.Pointer) int32
	optionsSetRelayMode func(h uint64, mode int32, outErr unsafe.Pointer) int32
	optionsAddRelayURL  func(h uint64, url unsafe.Pointer, urlLen uintptr, outErr unsafe.Pointer) int32
	optionsAddBindAddr  func(h uint64, addr unsafe.Pointer, addrLen uintptr, outErr unsafe.Pointer) int32

	endpointBind      func(options uint64) uint64
	endpointID        func(h uint64, outID, outErr unsafe.Pointer) int32
	endpointAddr      func(h uint64, outStr, outLen, outErr unsafe.Pointer) int32
	endpointSockets   func(h uint64, outStr, outLen, outErr unsafe.Pointer) int32
	endpointOnline    func(h uint64) uint64
	endpointHomeRelay func(h uint64, outStr, outLen, outErr unsafe.Pointer) int32
	endpointConnect   func(h uint64, addr unsafe.Pointer, addrLen uintptr, alpn unsafe.Pointer, alpnLen uintptr) uint64
	endpointAccept    func(h uint64) uint64
	endpointClose     func(h uint64) uint64
	endpointFree      func(h uint64)

	connOpenBi          func(h uint64) uint64
	connAcceptBi        func(h uint64) uint64
	connOpenUni         func(h uint64) uint64
	connAcceptUni       func(h uint64) uint64
	connSendDatagram    func(h uint64, data unsafe.Pointer, dataLen uintptr, outErr unsafe.Pointer) int32
	connReadDatagram    func(h uint64) uint64
	connMaxDatagramSize func(h uint64, outSize, outErr unsafe.Pointer) int32
	connRemoteID        func(h uint64, outID, outErr unsafe.Pointer) int32
	connALPN            func(h uint64, outALPN, outLen, outErr unsafe.Pointer) int32
	connStats           func(h uint64, out unsafe.Pointer, outCap uintptr, outErr unsafe.Pointer) int32
	connClose           func(h uint64, code uint64, reason unsafe.Pointer, reasonLen uintptr, outErr unsafe.Pointer) int32
	connClosed          func(h uint64) uint64
	connFree            func(h uint64)

	sendWrite       func(h uint64, data unsafe.Pointer, dataLen uintptr) uint64
	sendFinish      func(h uint64) uint64
	sendReset       func(h uint64, code uint64) uint64
	sendSetPriority func(h uint64, priority int32) uint64
	sendStopped     func(h uint64) uint64
	sendFree        func(h uint64)

	recvRead func(h uint64, maxLen uintptr) uint64
	recvStop func(h uint64, code uint64) uint64
	recvFree func(h uint64)
}

var (
	loadOnce sync.Once
	loadErr  error
	c        api
)

// Load opens the iroh library and binds every symbol. It is idempotent, and
// every exported call in this package invokes it first, so callers do not
// need to.
func Load() error {
	loadOnce.Do(func() {
		loadErr = load()
	})
	return loadErr
}

func load() (err error) {
	handle, err := lib.Open()
	if err != nil {
		return err
	}

	// RegisterLibFunc panics on a missing symbol. Turn that into an error so
	// a stale library reports what is wrong instead of aborting the process.
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("iroh: binding library symbols: %v", r)
		}
	}()

	for name, fptr := range symbols() {
		purego.RegisterLibFunc(fptr, handle, name)
	}

	if got := c.abiVersion(); got != ABIVersion {
		return fmt.Errorf("iroh: library ABI version %d, these bindings need %d", got, ABIVersion)
	}
	if c.initRuntime() != 0 {
		return fmt.Errorf("iroh: library failed to start its runtime")
	}
	startPump()
	return nil
}

func symbols() map[string]any {
	return map[string]any{
		"iroh_abi_version":         &c.abiVersion,
		"iroh_version":             &c.irohVersion,
		"iroh_init":                &c.initRuntime,
		"iroh_set_log_level":       &c.setLogLevel,
		"iroh_free_bytes":          &c.freeBytes,
		"iroh_debug_handle_count":  &c.handleCount,
		"iroh_completion_wait":     &c.completionWait,
		"iroh_completion_wake":     &c.completionWake,
		"iroh_op_cancel":           &c.opCancel,
		"iroh_op_free":             &c.opFree,
		"iroh_op_result_handle":    &c.opResultHandle,
		"iroh_op_result_handle2":   &c.opResultHandle2,
		"iroh_op_result_u64":       &c.opResultU64,
		"iroh_op_result_bytes":     &c.opResultBytes,
		"iroh_op_result_err":       &c.opResultErr,
		"iroh_error_take":          &c.errorTake,
		"iroh_secret_key_generate": &c.secretKeyGenerate,
		"iroh_secret_key_public":   &c.secretKeyPublic,
		"iroh_secret_key_sign":     &c.secretKeySign,
		"iroh_endpoint_id_verify":  &c.endpointIDVerify,
		"iroh_endpoint_id_format":  &c.endpointIDFormat,
		"iroh_endpoint_id_parse":   &c.endpointIDParse,
		"iroh_addr_normalize":      &c.addrNormalize,
		"iroh_ticket_encode":       &c.ticketEncode,
		"iroh_ticket_decode":       &c.ticketDecode,

		"iroh_options_new":            &c.optionsNew,
		"iroh_options_free":           &c.optionsFree,
		"iroh_options_set_secret_key": &c.optionsSetSecretKey,
		"iroh_options_add_alpn":       &c.optionsAddALPN,
		"iroh_options_set_relay_mode": &c.optionsSetRelayMode,
		"iroh_options_add_relay_url":  &c.optionsAddRelayURL,
		"iroh_options_add_bind_addr":  &c.optionsAddBindAddr,
		"iroh_endpoint_bind":          &c.endpointBind,
		"iroh_endpoint_id":            &c.endpointID,
		"iroh_endpoint_addr":          &c.endpointAddr,
		"iroh_endpoint_bound_sockets": &c.endpointSockets,
		"iroh_endpoint_online":        &c.endpointOnline,
		"iroh_endpoint_home_relay":    &c.endpointHomeRelay,
		"iroh_endpoint_connect":       &c.endpointConnect,
		"iroh_endpoint_accept":        &c.endpointAccept,
		"iroh_endpoint_close":         &c.endpointClose,
		"iroh_endpoint_free":          &c.endpointFree,
		"iroh_conn_open_bi":           &c.connOpenBi,
		"iroh_conn_accept_bi":         &c.connAcceptBi,
		"iroh_conn_open_uni":          &c.connOpenUni,
		"iroh_conn_accept_uni":        &c.connAcceptUni,
		"iroh_conn_send_datagram":     &c.connSendDatagram,
		"iroh_conn_read_datagram":     &c.connReadDatagram,
		"iroh_conn_max_datagram_size": &c.connMaxDatagramSize,
		"iroh_conn_remote_id":         &c.connRemoteID,
		"iroh_conn_alpn":              &c.connALPN,
		"iroh_conn_stats":             &c.connStats,
		"iroh_conn_close":             &c.connClose,
		"iroh_conn_closed":            &c.connClosed,
		"iroh_conn_free":              &c.connFree,
		"iroh_send_write":             &c.sendWrite,
		"iroh_send_finish":            &c.sendFinish,
		"iroh_send_reset":             &c.sendReset,
		"iroh_send_set_priority":      &c.sendSetPriority,
		"iroh_send_stopped":           &c.sendStopped,
		"iroh_send_free":              &c.sendFree,
		"iroh_recv_read":              &c.recvRead,
		"iroh_recv_stop":              &c.recvStop,
		"iroh_recv_free":              &c.recvFree,
	}
}
