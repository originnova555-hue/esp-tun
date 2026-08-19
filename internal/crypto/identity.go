package crypto

import (
	"crypto"
	"crypto/ed25519"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"fmt"
	"math/big"
	"time"
)

// There is no certificate authority anywhere in this design, and no need for
// one: both ends already share a secret, so both ends can derive the *same*
// key pair from it and each can check that the peer presented exactly that
// public key. A pre-shared key is doing the work a PKI would, which suits a
// two-party tunnel where the operator controls both sides.
//
// Note what this does and does not give you. It authenticates the peer as
// "someone who holds the PSK", which for a point-to-point tunnel is the whole
// question. It gives no forward secrecy on its own — that comes from the QUIC
// handshake's ephemeral X25519 exchange underneath, which happens regardless.

const (
	identityInfo = "quiccochet/v1/identity"

	// DefaultSNI is what the client asks for. Nothing verifies it, so its only
	// job is to look ordinary to whatever is watching the handshake.
	DefaultSNI = "www.cloudflare.com"
	// DefaultALPN makes the handshake look like HTTP/3, which is what the
	// overwhelming majority of QUIC on the public internet is.
	DefaultALPN = "h3"
)

// certValidity is fixed rather than relative to now, so both sides derive the
// same certificate no matter when they start.
var (
	certNotBefore = time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	certNotAfter  = time.Date(2040, 1, 1, 0, 0, 0, 0, time.UTC)
)

// Identity is the key pair and certificate both ends derive from the shared key.
type Identity struct {
	priv ed25519.PrivateKey
	pub  ed25519.PublicKey
	cert tls.Certificate
}

// DeriveIdentity turns a pre-shared key into a deterministic key pair and a
// self-signed certificate. Both ends of a tunnel produce the identical result.
func DeriveIdentity(psk, sni string) (*Identity, error) {
	if psk == "" {
		return nil, fmt.Errorf("crypto: empty pre-shared key")
	}
	if sni == "" {
		sni = DefaultSNI
	}
	seed, err := hkdf.Key(sha256.New, []byte(psk), nil, identityInfo, ed25519.SeedSize)
	if err != nil {
		return nil, fmt.Errorf("crypto: derive identity: %w", err)
	}
	priv := ed25519.NewKeyFromSeed(seed)
	pub := priv.Public().(ed25519.PublicKey)

	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: sni},
		DNSNames:              []string{sni},
		NotBefore:             certNotBefore,
		NotAfter:              certNotAfter,
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, pub, priv)
	if err != nil {
		return nil, fmt.Errorf("crypto: build certificate: %w", err)
	}
	return &Identity{
		priv: priv,
		pub:  pub,
		cert: tls.Certificate{
			Certificate: [][]byte{der},
			PrivateKey:  priv,
			Leaf:        mustParse(der),
		},
	}, nil
}

func mustParse(der []byte) *x509.Certificate {
	c, err := x509.ParseCertificate(der)
	if err != nil {
		return nil
	}
	return c
}

// PublicKey exposes the derived public key, for logging a short fingerprint.
func (id *Identity) PublicKey() ed25519.PublicKey { return id.pub }

// Fingerprint is a short, stable label for the shared identity. Printing it on
// both sides is the quickest way to confirm the two configs carry the same PSK.
func (id *Identity) Fingerprint() string {
	sum := sha256.Sum256(id.pub)
	return fmt.Sprintf("%x", sum[:6])
}

// verifyPeer accepts exactly one peer: the holder of the same pre-shared key.
func (id *Identity) verifyPeer(rawCerts [][]byte, _ [][]*x509.Certificate) error {
	if len(rawCerts) == 0 {
		return fmt.Errorf("crypto: peer presented no certificate")
	}
	cert, err := x509.ParseCertificate(rawCerts[0])
	if err != nil {
		return fmt.Errorf("crypto: peer certificate: %w", err)
	}
	got, ok := cert.PublicKey.(ed25519.PublicKey)
	if !ok {
		return fmt.Errorf("crypto: peer key is %T, want ed25519", cert.PublicKey)
	}
	if subtle.ConstantTimeCompare(got, id.pub) != 1 {
		return fmt.Errorf("crypto: peer does not hold the same pre-shared key")
	}
	return nil
}

// TLSOptions carries what the caller wants beyond the identity itself.
type TLSOptions struct {
	SNI    string
	ALPN   []string
	Cipher string // Auto, AESGCM or ChaCha20
}

func (o TLSOptions) alpn() []string {
	if len(o.ALPN) > 0 {
		return o.ALPN
	}
	return []string{DefaultALPN}
}

// ServerTLS builds the listening side's TLS config: present the derived
// certificate, and require the peer to present the same one.
func (id *Identity) ServerTLS(o TLSOptions) *tls.Config {
	return &tls.Config{
		Certificates:          []tls.Certificate{id.cert},
		MinVersion:            tls.VersionTLS13,
		NextProtos:            o.alpn(),
		ClientAuth:            tls.RequireAnyClientCert,
		VerifyPeerCertificate: id.verifyPeer,
		VerifyConnection:      cipherGuard(o.Cipher),
	}
}

// ClientTLS builds the dialling side's TLS config. Hostname verification is
// off on purpose — there is no name to verify against, only a key.
func (id *Identity) ClientTLS(o TLSOptions) *tls.Config {
	sni := o.SNI
	if sni == "" {
		sni = DefaultSNI
	}
	return &tls.Config{
		Certificates:          []tls.Certificate{id.cert},
		MinVersion:            tls.VersionTLS13,
		NextProtos:            o.alpn(),
		ServerName:            sni,
		InsecureSkipVerify:    true, // the key check below is the real gate
		VerifyPeerCertificate: id.verifyPeer,
		VerifyConnection:      cipherGuard(o.Cipher),
	}
}

// cipherGuard enforces an explicitly requested AEAD.
//
// TLS 1.3 cipher suites are not selectable through crypto/tls: the standard
// library picks AES-GCM when the CPU has hardware AES and ChaCha20-Poly1305
// otherwise, which is already the right answer and is what "auto" leaves
// alone. When an operator names a cipher explicitly, the honest thing is to
// check what was actually negotiated and refuse the connection if it differs,
// rather than accept something other than what was asked for.
func cipherGuard(want string) func(tls.ConnectionState) error {
	var suite uint16
	switch want {
	case AESGCM:
		suite = tls.TLS_AES_256_GCM_SHA384
	case ChaCha20:
		suite = tls.TLS_CHACHA20_POLY1305_SHA256
	default:
		return nil
	}
	return func(cs tls.ConnectionState) error {
		if cs.CipherSuite != suite {
			return fmt.Errorf(
				"crypto: negotiated %s but the config asks for %s; "+
					"TLS 1.3 suites follow the client's hardware, so set cipher = \"auto\" "+
					"or make both ends agree",
				tls.CipherSuiteName(cs.CipherSuite), tls.CipherSuiteName(suite))
		}
		return nil
	}
}

var _ crypto.Signer = ed25519.PrivateKey(nil)
