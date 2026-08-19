// Package crypto handles AEAD selection and the pre-shared-key identity that
// replaces a PKI on both ends of the tunnel.
package crypto

import (
	"fmt"
	"runtime"

	"golang.org/x/sys/cpu"
)

// Cipher names as they appear in config.
const (
	Auto     = "auto"
	AESGCM   = "aes-256-gcm"
	ChaCha20 = "chacha20-poly1305"
)

// HardwareAES reports whether this CPU can run AES-GCM on dedicated
// instructions. Without it, AES in software is several times slower than
// ChaCha20 and picking AES would be a straight loss.
func HardwareAES() bool {
	switch runtime.GOARCH {
	case "amd64":
		// GHASH needs carry-less multiply; AES-NI alone is not enough for
		// the constant-time GCM path.
		return cpu.X86.HasAES && cpu.X86.HasPCLMULQDQ
	case "arm64":
		return cpu.ARM64.HasAES && cpu.ARM64.HasPMULL
	case "s390x":
		return cpu.S390X.HasAES && cpu.S390X.HasAESCBC
	case "ppc64", "ppc64le":
		return cpu.PPC64.IsPOWER8
	default:
		return false
	}
}

// Choice records which AEAD will be used and why.
type Choice struct {
	Cipher      string // AESGCM or ChaCha20
	HardwareAES bool
	Requested   string // what the config asked for
	Reason      string
}

// Select resolves the configured cipher against what the CPU can actually do.
func Select(requested string) Choice {
	hw := HardwareAES()
	c := Choice{HardwareAES: hw, Requested: requested}
	switch requested {
	case AESGCM:
		c.Cipher = AESGCM
		if hw {
			c.Reason = "requested explicitly; this CPU has hardware AES"
		} else {
			c.Reason = "requested explicitly, but this CPU has no hardware AES — " +
				"chacha20-poly1305 would be considerably faster here"
		}
	case ChaCha20:
		c.Cipher = ChaCha20
		c.Reason = "requested explicitly"
	default: // Auto or empty
		if hw {
			c.Cipher = AESGCM
			c.Reason = "auto: this CPU has hardware AES, so AES-256-GCM is the faster AEAD"
		} else {
			c.Cipher = ChaCha20
			c.Reason = "auto: no hardware AES on this CPU, so ChaCha20-Poly1305 is the faster AEAD"
		}
	}
	return c
}

// String renders the choice for logs and the cpuinfo command.
func (c Choice) String() string {
	return fmt.Sprintf("%s (%s)", c.Cipher, c.Reason)
}
