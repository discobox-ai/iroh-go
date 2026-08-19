package iroh

import (
	"encoding/hex"

	"github.com/discobox-ai/iroh-go/internal/ffi"
)

// Sizes of the fixed-width key types, in bytes.
const (
	SecretKeySize  = 32
	EndpointIDSize = 32
	SignatureSize  = 64
)

// SecretKey is an endpoint's Ed25519 private key.
//
// It is a value type, so it can be stored and copied freely. Persist it with
// the raw bytes and restore with a plain conversion.
type SecretKey [SecretKeySize]byte

// GenerateSecretKey returns a new random secret key.
func GenerateSecretKey() (SecretKey, error) {
	var key SecretKey
	if err := ffi.SecretKeyGenerate(key[:]); err != nil {
		return key, err
	}
	return key, nil
}

// Public derives the endpoint id this key identifies.
func (k SecretKey) Public() (EndpointID, error) {
	var id EndpointID
	if err := ffi.SecretKeyPublic(k[:], id[:]); err != nil {
		return id, err
	}
	return id, nil
}

// Sign signs msg.
func (k SecretKey) Sign(msg []byte) (Signature, error) {
	var sig Signature
	if err := ffi.SecretKeySign(k[:], msg, sig[:]); err != nil {
		return sig, err
	}
	return sig, nil
}

// EndpointID is the public identity of an iroh endpoint: an Ed25519 public
// key that doubles as its network address.
type EndpointID [EndpointIDSize]byte

// ParseEndpointID accepts either the lowercase hex form that [EndpointID.String]
// produces or the z-base-32 form used in pkarr domain names, matching what
// iroh itself accepts. It also verifies that the bytes are a valid key.
func ParseEndpointID(s string) (EndpointID, error) {
	var id EndpointID
	if err := ffi.EndpointIDParse(s, id[:]); err != nil {
		return id, err
	}
	return id, nil
}

// String returns the lowercase hex form, which is iroh's canonical display
// encoding.
func (id EndpointID) String() string { return hex.EncodeToString(id[:]) }

// Short returns the abbreviated form iroh uses in logs.
func (id EndpointID) Short() string { return hex.EncodeToString(id[:5]) }

// IsZero reports whether the id is the zero value.
func (id EndpointID) IsZero() bool { return id == EndpointID{} }

// Verify checks a signature over msg. It returns nil if the signature is
// valid.
func (id EndpointID) Verify(msg []byte, sig Signature) error {
	return ffi.EndpointIDVerify(id[:], msg, sig[:])
}

// Signature is an Ed25519 signature.
type Signature [SignatureSize]byte

// String returns the lowercase hex form.
func (s Signature) String() string { return hex.EncodeToString(s[:]) }
